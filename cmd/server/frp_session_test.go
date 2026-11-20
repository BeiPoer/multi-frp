package main

import (
	"io"
	"strings"
	"testing"

	"github.com/fatedier/frp/server"
)

func TestFRPWorkConnectionRequiresLoginSession(t *testing.T) {
	loginSession := io.NopCloser(strings.NewReader("login"))
	otherSession := io.NopCloser(strings.NewReader("other"))

	if err := server.RequireWorkConnLoginSession(loginSession, loginSession); err != nil {
		t.Fatalf("work stream on the login session was refused: %v", err)
	}
	if err := server.RequireWorkConnLoginSession(loginSession, otherSession); err == nil {
		t.Fatal("work stream on a separate session was accepted")
	}
	if err := server.RequireWorkConnLoginSession(loginSession, nil); err == nil {
		t.Fatal("work stream without a transport session was accepted")
	}
	if err := server.RequireWorkConnLoginSession(nil, nil); err == nil {
		t.Fatal("missing login and work sessions were accepted")
	}
}
