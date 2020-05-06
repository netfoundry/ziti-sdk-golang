/*
	Copyright 2019 NetFoundry, Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package edge_impl

import (
	"fmt"
	"github.com/michaelquigley/pfxlog"
	"github.com/netfoundry/ziti-foundation/util/concurrenz"
	"github.com/netfoundry/ziti-sdk-golang/ziti/edge"
	"github.com/pkg/errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"
)

type baseListener struct {
	serviceName string
	acceptC     chan net.Conn
	errorC      chan error
	closed      concurrenz.AtomicBoolean
}

func (listener *baseListener) Network() string {
	return "ziti"
}

func (listener *baseListener) String() string {
	return listener.serviceName
}

func (listener *baseListener) Addr() net.Addr {
	return listener
}

func (listener *baseListener) Accept() (net.Conn, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for !listener.closed.Get() {
		select {
		case conn, ok := <-listener.acceptC:
			if ok && conn != nil {
				return conn, nil
			} else {
				listener.closed.Set(true)
			}
		case <-ticker.C:
		}
	}

	select {
	case err := <-listener.errorC:
		return nil, fmt.Errorf("listener is closed (%w)", err)
	default:
	}

	return nil, errors.New("listener is closed")
}

type edgeListener struct {
	baseListener
	token    string
	edgeChan *edgeConn
}

func (listener *edgeListener) UpdateCost(cost uint16) error {
	return listener.updateCostAndPrecedence(&cost, nil)
}

func (listener *edgeListener) UpdatePrecedence(precedence edge.Precedence) error {
	return listener.updateCostAndPrecedence(nil, &precedence)
}

func (listener *edgeListener) UpdateCostAndPrecedence(cost uint16, precedence edge.Precedence) error {
	return listener.updateCostAndPrecedence(&cost, &precedence)
}

func (listener *edgeListener) updateCostAndPrecedence(cost *uint16, precedence *edge.Precedence) error {
	logger := pfxlog.Logger().
		WithField("connId", listener.edgeChan.Id()).
		WithField("service", listener.edgeChan.serviceId).
		WithField("session", listener.token)

	logger.Debug("sending update bind request to edge router")
	request := edge.NewUpdateBindMsg(listener.edgeChan.Id(), listener.token, cost, precedence)
	listener.edgeChan.TraceMsg("updateCostAndPrecedence", request)
	return listener.edgeChan.Send(request)
}

func (listener *edgeListener) Close() error {
	if !listener.closed.CompareAndSwap(false, true) {
		// already closed
		return nil
	}

	listener.edgeChan.hosting.Delete(listener.token)

	edgeChan := listener.edgeChan
	defer func() {
		if err := edgeChan.Close(); err != nil {
			pfxlog.Logger().Errorf("unable to close edgeConn with connId %v", edgeChan.Id())
		}

		listener.acceptC <- nil // signal listeners that listener is closed
	}()

	unbindRequest := edge.NewUnbindMsg(edgeChan.Id(), listener.token)
	if err := edgeChan.SendWithTimeout(unbindRequest, time.Second*5); err != nil {
		pfxlog.Logger().Errorf("unable to unbind session %v for connId %v", listener.token, edgeChan.Id())
		return err
	}

	return nil
}

type MultiListener interface {
	edge.Listener
	AddListener(listener edge.Listener, closeHandler func())
	IsClosed() bool
	GetServiceName() string
	CloseWithError(err error)
}

func NewMultiListener(serviceName string) MultiListener {
	return &multiListener{
		baseListener: baseListener{
			serviceName: serviceName,
			acceptC:     make(chan net.Conn),
			errorC:      make(chan error),
		},
		listeners: map[edge.Listener]struct{}{},
	}
}

type multiListener struct {
	baseListener
	listeners    map[edge.Listener]struct{}
	listenerLock sync.Mutex
}

func (listener *multiListener) UpdateCost(cost uint16) error {
	listener.listenerLock.Lock()
	defer listener.listenerLock.Unlock()

	var errors []error
	for child := range listener.listeners {
		if err := child.UpdateCost(cost); err != nil {
			errors = append(errors, err)
		}
	}
	return listener.condenseErrors(errors)
}

func (listener *multiListener) UpdatePrecedence(precedence edge.Precedence) error {
	listener.listenerLock.Lock()
	defer listener.listenerLock.Unlock()

	var errors []error
	for child := range listener.listeners {
		if err := child.UpdatePrecedence(precedence); err != nil {
			errors = append(errors, err)
		}
	}
	return listener.condenseErrors(errors)
}

func (listener *multiListener) UpdateCostAndPrecedence(cost uint16, precedence edge.Precedence) error {
	listener.listenerLock.Lock()
	defer listener.listenerLock.Unlock()

	var errors []error
	for child := range listener.listeners {
		if err := child.UpdateCostAndPrecedence(cost, precedence); err != nil {
			errors = append(errors, err)
		}
	}
	return listener.condenseErrors(errors)
}

func (listener *multiListener) condenseErrors(errors []error) error {
	if len(errors) == 0 {
		return nil
	}
	if len(errors) == 1 {
		return errors[0]
	}
	return MultipleErrors(errors)
}

func (listener *multiListener) GetServiceName() string {
	return listener.serviceName
}

func (listener *multiListener) IsClosed() bool {
	return listener.closed.Get()
}

func (listener *multiListener) AddListener(netListener edge.Listener, closeHandler func()) {
	if listener.closed.Get() {
		return
	}

	wrappedListener, ok := netListener.(*edgeListener)
	if !ok {
		pfxlog.Logger().Errorf("multi-listener expects only listeners created by the SDK, not %v", reflect.TypeOf(listener))
		return
	}

	listener.listenerLock.Lock()
	defer listener.listenerLock.Unlock()
	listener.listeners[wrappedListener] = struct{}{}

	closer := func() {
		listener.listenerLock.Lock()
		defer listener.listenerLock.Unlock()
		delete(listener.listeners, wrappedListener)

		closeHandler()
	}

	go listener.forward(wrappedListener, closer)
}

func (listener *multiListener) forward(edgeListener *edgeListener, closeHandler func()) {
	defer func() {
		if err := edgeListener.Close(); err != nil {
			pfxlog.Logger().Errorf("failure closing edge listener: (%v)", err)
		}
		closeHandler()
	}()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for !listener.closed.Get() && !edgeListener.closed.Get() {
		select {
		case conn, ok := <-edgeListener.acceptC:
			if !ok || conn == nil {
				// closed, returning
				return
			}
			listener.accept(conn, ticker)
		case <-ticker.C:
			// lets us check if the listener is closed, and exit if it has
		}
	}
}

func (listener *multiListener) accept(conn net.Conn, ticker *time.Ticker) {
	for !listener.closed.Get() {
		select {
		case listener.acceptC <- conn:
			return
		case <-ticker.C:
			// lets us check if the listener is closed, and exit if it has
		}
	}
}

func (listener *multiListener) Close() error {
	listener.closed.Set(true)
	return nil
}

func (listener *multiListener) CloseWithError(err error) {
	select {
	case listener.errorC <- err:
	default:
	}

	listener.closed.Set(true)
}

type MultipleErrors []error

func (e MultipleErrors) Error() string {
	if len(e) == 0 {
		return "no errors occurred"
	}
	if len(e) == 1 {
		return e[0].Error()
	}
	buf := strings.Builder{}
	buf.WriteString("multiple errors occurred")
	for idx, err := range e {
		buf.WriteString(fmt.Sprintf(" %v: %v", idx, err))
	}
	return buf.String()
}
