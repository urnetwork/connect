//go:build linux || android

package connect

import (
	"encoding/binary"
	"sync"

	"golang.org/x/sys/unix"
)

type udpSocketEpollBackend struct {
	mutex    sync.Mutex
	fd       int
	wakeFd   int
	osEvents [udpSocketPollEventCount]unix.EpollEvent
}

func newUdpSocketPollBackend() (udpSocketPollBackend, error) {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	wakeFd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.EpollCtl(fd, unix.EPOLL_CTL_ADD, wakeFd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(wakeFd)}); err != nil {
		_ = unix.Close(wakeFd)
		_ = unix.Close(fd)
		return nil, err
	}
	return &udpSocketEpollBackend{fd: fd, wakeFd: wakeFd}, nil
}

func (self *udpSocketEpollBackend) add(fd int) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return unix.EpollCtl(self.fd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLERR | unix.EPOLLHUP,
		Fd:     int32(fd),
	})
}

func (self *udpSocketEpollBackend) remove(fd int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	_ = unix.EpollCtl(self.fd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (self *udpSocketEpollBackend) wake() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.wakeFd < 0 {
		return
	}
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	_, _ = unix.Write(self.wakeFd, value[:])
}

func (self *udpSocketEpollBackend) wait(events []udpSocketPollEvent) (int, error) {
	n, err := unix.EpollWait(self.fd, self.osEvents[:min(len(events), len(self.osEvents))], 100)
	for i := range n {
		event := self.osEvents[i]
		if int(event.Fd) == self.wakeFd {
			var value [8]byte
			_, _ = unix.Read(self.wakeFd, value[:])
			events[i] = udpSocketPollEvent{fd: -1}
			continue
		}
		events[i] = udpSocketPollEvent{
			fd:       int(event.Fd),
			terminal: event.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0,
		}
	}
	return n, err
}

func (self *udpSocketEpollBackend) close() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.fd < 0 {
		return nil
	}
	_ = unix.Close(self.wakeFd)
	self.wakeFd = -1
	err := unix.Close(self.fd)
	self.fd = -1
	return err
}
