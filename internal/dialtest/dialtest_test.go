package dialtest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
)

func TestDialRefused(t *testing.T) {
	_, err := Dial(context.Background(), "tcp", Refused)
	var op *net.OpError
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("Dial: %v", err)
	}
}

func TestDialOpen(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := Dial(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestClientRefused(t *testing.T) {
	_, err := Client().Get("http://" + Refused + "/")
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Get: %v", err)
	}
}

func TestHangUp(t *testing.T) {
	resp, err := http.Get(HangUp(t))
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("unexpected response: %v", resp)
	}
	if err == nil {
		t.Fatal("expected transport error")
	}
}
