package session

import (
	"net"
	"testing"
	"time"
)

// pair is a TCP connection and its other end.
func pair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	t.Cleanup(func() { server.Close(); client.Close() })
	return server, client
}

// discard reads frames from c until it fails, and closes the returned channel then.
func discard(c net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := ReadFrame(c); err != nil {
				return
			}
		}
	}()
	return done
}

func TestEndLetsAPeerStillSendingReadTheLastFrame(t *testing.T) {
	server, client := pair(t)
	stop := make(chan struct{})
	sending := make(chan struct{})
	go func() {
		defer close(sending)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if WriteFrame(client, Stdin, make([]byte, 4096)) != nil {
				return
			}
		}
	}()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = WriteFrame(server, Exit, []byte(`{"code":3}`))
		End(server, discard(server))
	}()
	if k, p, err := ReadFrame(client); err != nil || k != Exit || string(p) != `{"code":3}` {
		t.Fatalf("frame %d %q %v", k, p, err)
	}
	close(stop)
	<-sending
	select {
	case <-ended:
		t.Fatal("End returned before the peer closed")
	case <-time.After(50 * time.Millisecond):
	}
	client.Close()
	<-ended
}

func TestEndGivesUpOnAPeerThatNeverCloses(t *testing.T) {
	old := endTime
	endTime = 50 * time.Millisecond
	t.Cleanup(func() { endTime = old })
	server, client := pair(t)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		End(server, discard(server))
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("End waited for a peer that never closes")
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection is still open")
	}
}
