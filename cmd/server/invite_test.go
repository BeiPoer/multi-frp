package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"multifrp/internal/control"
)

func TestInvitationHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, _, err := openStore(path, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	oldCode, expiredCode := randomID(16), randomID(16)
	legacy, err := json.Marshal(map[string]any{
		"domain": "localhost", "tunnels": map[string]control.Tunnel{},
		"devices": map[string]device{"existing": {ID: "existing", Name: "Existing"}},
		"invites": map[string]time.Time{digest(oldCode): now.Add(time.Hour), digest(expiredCode): now.Add(-time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket(stateBucket).Put(stateKey, legacy) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, data, err := openStore(path, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway("127.0.0.1", 30000, 30099)
	defer g.close()
	a := &app{db: db, data: data, gateway: g, zone: time.UTC,
		sessions: map[string]time.Time{"test-admin": now.Add(time.Hour)}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/state", a.admin(a.dashboard))
	mux.HandleFunc("POST /api/admin/invites", a.admin(a.invite))
	mux.HandleFunc("POST /api/admin/invites/{id}/remark", a.admin(a.updateInviteRemark))
	mux.HandleFunc("POST /api/admin/devices/{id}/revoke", a.admin(a.revoke))
	mux.HandleFunc("POST /api/admin/devices/{id}/delete", a.admin(a.deleteDevice))
	mux.HandleFunc("POST /api/client/enroll", a.enroll)
	mux.HandleFunc("GET /api/client/state", a.clientState)
	call := func(method, path string, body any, session bool, bearer string, status int) []byte {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
		if session {
			r.AddCookie(&http.Cookie{Name: "multifrp_session", Value: "test-admin"})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got status %d, want %d: %s", method, path, w.Code, status, w.Body)
		}
		return w.Body.Bytes()
	}
	const adminState = "/api/admin/state"
	const generate = "/api/admin/invites"
	const enroll = "/api/client/enroll"
	call(http.MethodGet, adminState, nil, false, "", 401)
	createInvite := func(remark string) invitationView {
		t.Helper()
		raw := call(http.MethodPost, generate, map[string]string{"remark": remark}, true, "", 201)
		var entry invitationView
		if err := json.Unmarshal(raw, &entry); err != nil || entry.ID == "" || len(entry.Code) != 32 || entry.Remark != remark {
			t.Fatalf("unexpected invitation: %s (%v)", raw, err)
		}
		return entry
	}
	first := createInvite("仓库终端")
	second := createInvite("")
	consume := func(code string) control.Enrollment {
		t.Helper()
		raw := call(http.MethodPost, enroll, map[string]string{"code": code, "name": "Terminal"}, false, "", 201)
		var credentials control.Enrollment
		if err := json.Unmarshal(raw, &credentials); err != nil || credentials.DeviceID == "" {
			t.Fatalf("unexpected enrollment: %s (%v)", raw, err)
		}
		return credentials
	}
	linked := consume(first.Code)
	legacyLinked := consume(oldCode)
	for _, code := range []string{first.Code, oldCode, expiredCode} {
		call(http.MethodPost, enroll, map[string]string{"code": code, "name": "Another"}, false, "", 403)
	}
	remarkPath := generate + "/" + first.ID + "/remark"
	call(http.MethodPost, remarkPath, map[string]string{"remark": "unauthorized"}, false, "", 401)
	call(http.MethodPost, remarkPath, map[string]string{"remark": strings.Repeat("备", 201)}, true, "", 400)
	call(http.MethodPost, remarkPath, map[string]string{"remark": "库房已启用"}, true, "", 200)
	client := call(http.MethodGet, "/api/client/state", nil, false, linked.DeviceID+"."+linked.Token, 200)
	if bytes.Contains(client, []byte(first.Code)) || bytes.Contains(client, []byte("库房已启用")) {
		t.Fatal("invite data leaked into the client API")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, data, err = openStore(path, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	a.db, a.data = db, data
	defer db.Close()
	var dashboard struct {
		Invites []invitationView
		Devices []device
	}
	raw := call(http.MethodGet, adminState, nil, true, "", 200)
	if err := json.Unmarshal(raw, &dashboard); err != nil || len(dashboard.Invites) != 4 || len(dashboard.Devices) != 3 {
		t.Fatalf("incomplete invitation history: %s (%v)", raw, err)
	}
	entries := map[string]invitationView{}
	for _, entry := range dashboard.Invites {
		entries[entry.ID] = entry
	}
	if entry := entries[first.ID]; entry.Code != first.Code || entry.Remark != "库房已启用" || entry.DeviceID != linked.DeviceID || entry.Status != "used" {
		t.Fatalf("incorrect used invite: %+v", entry)
	}
	if entry := entries[second.ID]; entry.Code != second.Code || entry.Status != "unused" {
		t.Fatalf("incorrect unused invite: %+v", entry)
	}
	if entry := entries[digest(expiredCode)]; entry.Code != "" || entry.Status != "expired" {
		t.Fatalf("incorrect legacy expired invite: %+v", entry)
	}
	if entry := entries[digest(oldCode)]; entry.Code != oldCode || entry.DeviceID != legacyLinked.DeviceID || entry.Status != "used" {
		t.Fatalf("legacy invite redemption not recorded: %+v", entry)
	}
	for _, item := range dashboard.Devices {
		if item.ID == linked.DeviceID && item.InviteID != first.ID {
			t.Fatal("device did not retain its invitation link")
		}
		if item.ID == "existing" && item.InviteID != "" {
			t.Fatal("legacy device gained an incorrect invitation link")
		}
	}
	devicePath := "/api/admin/devices/" + linked.DeviceID
	call(http.MethodPost, devicePath+"/delete", struct{}{}, false, "", 401)
	call(http.MethodPost, devicePath+"/delete", struct{}{}, true, "", 409)
	call(http.MethodPost, devicePath+"/revoke", struct{}{}, true, "", 200)
	call(http.MethodPost, devicePath+"/delete", struct{}{}, true, "", 200)
	call(http.MethodPost, devicePath+"/delete", struct{}{}, true, "", 404)
	call(http.MethodPost, enroll, map[string]string{"code": first.Code, "name": "Another"}, false, "", 403)
}
