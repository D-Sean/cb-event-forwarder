package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type NetOutput struct {
	netConn        string
	remoteHostname string
	protocolName   string
	outputSocket   net.Conn
	addNewline     bool

	connectTime       time.Time
	reconnectTime     time.Time
	connected         bool
	droppedEventCount int64

	sync.RWMutex
}

type NetStatistics struct {
	LastOpenTime      time.Time `json:"last_open_time"`
	Protocol          string    `json:"connection_protocol"`
	RemoteHostname    string    `json:"remote_hostname"`
	DroppedEventCount int64     `json:"dropped_event_count"`
	Connected         bool      `json:"connected"`
}

// Initialize() expects a connection string in the following format:
// (protocol):(hostname/IP):(port)
// for example: tcp:destination.server.example.com:512
func (o *NetOutput) Initialize(netConn string) error {
	o.Lock()
	defer o.Unlock()

	if o.connected {
		o.outputSocket.Close()
	}

	o.netConn = netConn

	connSpecification := strings.SplitN(netConn, ":", 2)

	o.protocolName = connSpecification[0]
	o.remoteHostname = connSpecification[1]

	if strings.HasPrefix(o.protocolName, "tcp") {
		o.addNewline = true
	}

	var err error
	o.outputSocket, err = net.Dial(o.protocolName, o.remoteHostname)

	if err != nil {
		return errors.New(fmt.Sprintf("Error connecting to '%s': %s", netConn, err))
	}

	o.connectTime = time.Now()
	o.connected = true

	// we need a way to ensure that we don't block on the output. We will disconnect and reconnect if this timeout
	// occurs
	// TODO: Make sure this is correct!
	o.outputSocket.SetWriteDeadline(time.Time{}.Add(time.Duration(500 * time.Millisecond)))

	return nil
}

func (o *NetOutput) closeAndScheduleReconnection() {
	o.Lock()
	defer o.Unlock()

	if o.connected {
		o.outputSocket.Close()
	}

	// try reconnecting in 30 seconds
	o.reconnectTime = time.Now().Add(time.Duration(30*time.Second))
}

func (o *NetOutput) Key() string {
	o.RLock()
	defer o.RUnlock()

	return o.netConn
}

func (o *NetOutput) String() string {
	o.RLock()
	defer o.RUnlock()

	return o.netConn
}

func (o *NetOutput) Statistics() interface{} {
	o.RLock()
	defer o.RUnlock()

	return NetStatistics{
		LastOpenTime:      o.connectTime,
		Protocol:          o.protocolName,
		RemoteHostname:    o.remoteHostname,
		DroppedEventCount: o.droppedEventCount,
		Connected:         o.connected,
	}
}

func (o *NetOutput) output(m string) error {
	if o.addNewline {
		m = m + "\r\n"
	}

	if !o.connected {
		// drop this event on the floor...
		atomic.AddInt64(&o.droppedEventCount, 1)
		return nil
	}

	_, err := o.outputSocket.Write([]byte(m))
	if err != nil {
		// try to reconnect and send again
		err = o.Initialize(o.netConn)
		if err != nil {
			return err
		}
		_, err = o.outputSocket.Write([]byte(m))
		// if we still have an error...
		if err != nil {
			o.closeAndScheduleReconnection()
		}
		return err
	}
	return err
}

func (o *NetOutput) Go(messages <-chan string, errorChan chan<- error) error {
	if o.outputSocket == nil {
		return errors.New("Output socket not open")
	}

	go func() {
		refreshTicker := time.NewTicker(1 * time.Second)
		defer refreshTicker.Stop()

		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)

		defer signal.Stop(hup)

		for {
			select {
			case message := <-messages:
				if err := o.output(message); err != nil {
					errorChan <- err
					return
				}

			case <-refreshTicker.C:
				if !o.connected && time.Now().After(o.reconnectTime) {
					err := o.Initialize(o.netConn)
					if err != nil {
						o.closeAndScheduleReconnection()
					}
				}
			}
		}

	}()

	return nil
}
