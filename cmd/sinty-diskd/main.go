// sinty-diskd is the disk daemon of SintyOS. It publishes the drives, block
// devices, partitions and filesystems on org.freedesktop.UDisks2 and mounts,
// unmounts, ejects and powers off removable media for the user.
package main

import (
	"bytes"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
	"github.com/singularityos-lab/sinty-diskd/internal/service"
)

func blockEvents(changed func()) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: 2}); err != nil {
		syscall.Close(fd)
		return err
	}
	go func() {
		defer syscall.Close(fd)
		buf := make([]byte, 65536)
		for {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				return
			}
			if bytes.Contains(buf[:n], []byte("\x00SUBSYSTEM=block\x00")) {
				changed()
			}
		}
	}()
	return nil
}

func mountEvents(changed func()) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		f.Close()
		return err
	}
	ev := syscall.EpollEvent{Events: syscall.EPOLLPRI | syscall.EPOLLERR, Fd: int32(f.Fd())}
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, int(f.Fd()), &ev); err != nil {
		syscall.Close(ep)
		f.Close()
		return err
	}
	go func() {
		defer f.Close()
		defer syscall.Close(ep)
		events := make([]syscall.EpollEvent, 1)
		for {
			n, err := syscall.EpollWait(ep, events, -1)
			if err != nil {
				if err == syscall.EINTR {
					continue
				}
				return
			}
			if n > 0 {
				changed()
			}
		}
	}()
	return nil
}

func main() {
	udevData := flag.String("udev-data", "/run/udev/data", "udev database directory")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("sinty-diskd: ")
	if os.Getenv("DBUS_SYSTEM_BUS_ADDRESS") == "" {
		os.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/run/dbus/system_bus_socket")
	}
	src := model.DefaultSources()
	src.UdevData = *udevData
	var s *service.Service
	var err error
	for i := 0; ; i++ {
		if s, err = service.New(src); err == nil {
			break
		}
		if i%10 == 0 {
			log.Printf("system bus not ready: %v", err)
		}
		time.Sleep(time.Second)
	}
	s.Refresh()
	changes := make(chan struct{}, 1)
	notify := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	if err := blockEvents(notify); err != nil {
		log.Printf("block events: %v", err)
	}
	every := 5 * time.Second
	if err := mountEvents(notify); err != nil {
		log.Printf("mount events: %v", err)
	} else {
		every = 5 * time.Minute
	}
	log.Print("serving org.freedesktop.UDisks2")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	mounts := time.NewTicker(every)
	for {
		select {
		case <-changes:
			time.Sleep(300 * time.Millisecond)
			s.Refresh()
		case <-mounts.C:
			s.Refresh()
		case <-sigs:
			return
		}
	}
}
