//go:build darwin || ios

package connect

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type udpSocketKqueueBackend struct {
	mutex    sync.Mutex
	fd       int
	osEvents [udpSocketPollEventCount]unix.Kevent_t
}

func newUdpSocketPollBackend() (udpSocketPollBackend, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	change := unix.Kevent_t{Ident: 1, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &udpSocketKqueueBackend{fd: fd}, nil
}

func (self *udpSocketKqueueBackend) add(fd int) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	change := unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: unix.EVFILT_READ,
		Flags:  unix.EV_ADD | unix.EV_ENABLE,
	}
	_, err := unix.Kevent(self.fd, []unix.Kevent_t{change}, nil, nil)
	return err
}

func (self *udpSocketKqueueBackend) remove(fd int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	change := unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: unix.EVFILT_READ,
		Flags:  unix.EV_DELETE,
	}
	_, _ = unix.Kevent(self.fd, []unix.Kevent_t{change}, nil, nil)
}

func (self *udpSocketKqueueBackend) wake() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return
	}
	change := unix.Kevent_t{Ident: 1, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}
	_, _ = unix.Kevent(self.fd, []unix.Kevent_t{change}, nil, nil)
}

func (self *udpSocketKqueueBackend) wait(events []udpSocketPollEvent) (int, error) {
	timeout := unix.NsecToTimespec((100 * time.Millisecond).Nanoseconds())
	n, err := unix.Kevent(self.fd, nil, self.osEvents[:min(len(events), len(self.osEvents))], &timeout)
	for i := range n {
		event := self.osEvents[i]
		if event.Filter == unix.EVFILT_USER {
			events[i] = udpSocketPollEvent{fd: -1}
			continue
		}
		events[i] = udpSocketPollEvent{
			fd:       int(event.Ident),
			terminal: event.Flags&(unix.EV_ERROR|unix.EV_EOF) != 0,
		}
	}
	return n, err
}

func (self *udpSocketKqueueBackend) close() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return nil
	}
	err := unix.Close(self.fd)
	self.fd = -1
	return err
}
