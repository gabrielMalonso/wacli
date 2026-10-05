package main

import (
	"context"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
)

type agentAuth struct {
	Authenticated  bool   `json:"authenticated"`
	LinkedJID      string `json:"linked_jid,omitempty"`
	Phone          string `json:"phone,omitempty"`
	SessionRevoked bool   `json:"session_revoked"`
	Connected      string `json:"connected"`
}

func readAgentAuth(storeDir string) (agentAuth, error) {
	status := agentAuth{Connected: "unknown"}
	if info, err := os.Stat(storeDir); err != nil {
		return status, agentStoreError(err)
	} else if !info.IsDir() {
		return status, agentStoreError(os.ErrInvalid)
	}
	authed, jid, err := readOnlyAuthStatus(storeDir)
	if err != nil {
		return status, &out.AgentError{Code: "store_unavailable", Message: "Selected local session state cannot be read.", ExitCode: 4, Cause: err}
	}
	revoked, err := app.SessionRevoked(storeDir)
	if err != nil {
		return status, &out.AgentError{Code: "store_unavailable", Message: "Selected local session observation cannot be read.", ExitCode: 4, Cause: err}
	}
	status.Authenticated = authed
	status.LinkedJID = jid
	status.Phone = phoneFromLinkedJID(jid)
	status.SessionRevoked = revoked
	return status, nil
}
func runAgentAuthStatus(flags *rootFlags) error {
	dir, err := resolveStoreDir(flags)
	if err != nil {
		return agentStoreError(err)
	}
	status, err := readAgentAuth(dir)
	if err != nil {
		return err
	}
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, agentMeta(flags), status)
}

type agentDoctor struct {
	Auth           agentAuth               `json:"auth"`
	LockHeld       bool                    `json:"lock_held"`
	FTSEnabled     bool                    `json:"fts_enabled"`
	LastActivityAt *time.Time              `json:"last_activity_at"`
	Full           *agentDoctorFull        `json:"full,omitempty"`
	AppState       app.AppStateDiagnostics `json:"app_state"`
}
type agentDoctorFull struct {
	Messages      int64      `json:"messages"`
	Chats         int64      `json:"chats"`
	Contacts      int64      `json:"contacts"`
	Groups        int64      `json:"groups"`
	LastMessageAt *time.Time `json:"last_message_at"`
}

func runAgentDoctor(ctx context.Context, flags *rootFlags) error {
	a, lk, err := newReadApp(ctx, flags)
	if err != nil {
		return err
	}
	defer closeApp(a, lk)
	status, err := readAgentAuth(a.StoreDir())
	if err != nil {
		return err
	}
	held, _, err := lock.Probe(a.StoreDir())
	if err != nil {
		return agentStoreError(err)
	}
	data := agentDoctor{Auth: status, LockHeld: held, FTSEnabled: a.DB().HasFTS(), LastActivityAt: agentTime(app.ReadHeartbeat(a.StoreDir())), AppState: app.ReadAppStateDiagnostics(a.DB())}
	if flags.detail == "full" {
		stats, err := a.DB().Stats()
		if err != nil {
			return agentStoreError(err)
		}
		var last *time.Time
		if stats.LastMessageTS > 0 {
			last = agentTime(time.Unix(stats.LastMessageTS, 0))
		}
		data.Full = &agentDoctorFull{stats.Messages, stats.Chats, stats.Contacts, stats.Groups, last}
	}
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, agentMeta(flags), data)
}
