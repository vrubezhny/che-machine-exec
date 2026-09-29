//
// Copyright (c) 2026 Red Hat, Inc.
// This program and the accompanying materials are made
// available under the terms of the Eclipse Public License 2.0
// which is available at https://www.eclipse.org/legal/epl-2.0/
//
// SPDX-License-Identifier: EPL-2.0
//
// Contributors:
//   Red Hat, Inc. - initial API and implementation
//

package timeout

import (
	"fmt"
	"time"
)

// codexAppServerApiActivitySource detects activity on a codex app-server by
// connecting to its own `--listen unix://...` control socket as a plain,
// passive JSON-RPC client and polling `thread/loaded/list` + `thread/read`
// for any loaded thread whose runtime `status` is `active`. Unlike the
// hooks-based `codex-app-server-hooks` source (see
// activity_source_codex_app_server_hooks.go and
// timeout/codex-hooks/README.md), this needs no managed hooks, no
// /etc/codex provisioning, and no root/image-build-time step - only that
// che-machine-exec can reach the already-running app-server's socket,
// which it already needs to *find* via the shared /proc discovery in
// codex_app_server_discovery.go.
//
// This source never calls thread/start, thread/resume, or any method that
// would create or join a conversation - thread/loaded/list + thread/read
// are plain, unscoped request/response calls that report state for every
// thread currently loaded in the server's memory regardless of which
// client (if any) started or resumed it, so a fully passive observer is
// sufficient. `thread/list` was deliberately NOT used here despite
// looking like the more obvious choice: live-tested against a real
// app-server, it reflects the persisted/indexed thread store, not what's
// actually loaded/live right now - it omitted a thread that had just been
// created and was actively being driven in the very same moment. See
// codex_app_server_client.go's codexThreadLoadedListResponse doc comment
// and the codex-app-server-api plan (activity-sources-plan project
// memory) for the full investigation.
//
// A thread's `status` is *supposed* to stay `active` (not `idle`) for the
// whole time a conversation is open - including while codex is waiting on
// the user's next message (`activeFlags` can include `waitingOnUserInput`)
// - not just mid-tool-execution. But this has never actually been
// observed live through a successful turn (only live-verified up to a
// turn failing with a genuine auth error, where status correctly went
// `active` -> `systemError`, a real terminal state, not a detection
// miss). Given that assumption is unconfirmed and the cost of being
// wrong (a workspace idling out on someone still actively using codex)
// is high, ActivityWindow IS applied here after all, same convention as
// `tty` and the hooks source: a momentary "seen active" observation keeps
// this source reporting active for the rest of the configured window,
// not just the single scan cycle it was actually observed on - see
// lastActiveAt below. GracePeriod/MaxProcessAge remain deliberately NOT
// applied: there's no detection lag to compensate for (the first "seen
// active" observation IS a real-time activity event, same reasoning as
// the hooks source's SessionStart), and a codex-app-server daemon is
// expected to run for the whole workspace lifetime.
type codexAppServerApiActivitySource struct {
	// conns caches one persistent client connection per candidate pid
	// across scan ticks, to avoid paying the dial+initialize handshake
	// cost on every scan. Keyed by pid rather than socket path so a
	// restarted codex-app-server (new pid, possibly same socket path)
	// naturally gets a fresh connection instead of reusing a stale one.
	// Only ever read/written from Scan, which runs solely on the
	// watcher's single ticker-loop goroutine - no concurrent access, no
	// mutex needed (same reasoning as codexAppServerHooksActivitySource's
	// lastWarnedPID field).
	conns map[string]*codexAppServerClient

	// lastActiveAt records, per candidate pid, the last time any of its
	// loaded threads was observed with status "active". Scan reports
	// active if either a thread is active right now, OR the most recent
	// "active" observation is still within ctx.ActivityWindow - same
	// window-extension convention as the tty source's TTY-atime check and
	// the hooks source's status-file-mtime check, needed here for the
	// same reason: a single poll landing exactly in a real (but benign)
	// gap between "actively streaming a response" and "waiting on the
	// user's next message" must not look like the conversation ended.
	lastActiveAt map[string]time.Time
}

// newCodexAppServerApiActivitySource constructs the codex-app-server-api
// ActivitySource.
func newCodexAppServerApiActivitySource() ActivitySource {
	return &codexAppServerApiActivitySource{}
}

// Name returns the canonical identifier for this source.
func (s *codexAppServerApiActivitySource) Name() string {
	return "codex-app-server-api"
}

// Scan looks for running codex-app-server processes and reports active as
// soon as any one reports at least one thread with status "active".
// Multiple candidates are checked, not just the first found, for the same
// reason as the hooks source: distinct instances can be genuinely
// distinct servers, so a quiet first instance must not hide activity on
// another.
func (s *codexAppServerApiActivitySource) Scan(ctx ActivityScanContext) (bool, string) {
	candidates := findCodexAppServerProcesses(ctx.MyPID)
	s.pruneStaleConnections(candidates)

	for _, cand := range candidates {
		if active, label := s.scanCandidate(cand, ctx); active {
			return true, label
		}
	}

	return false, ""
}

// scanCandidate connects to (or reuses a connection to) one candidate's
// control socket and checks every currently-loaded thread for one whose
// status is active. In verbose mode this logs its full reasoning - not
// just the final active/not-active outcome - since (unlike the tty and
// hooks sources) there's no other way for an admin to sanity-check "is
// this source even seeing my app-server" without reaching for a raw
// JSON-RPC client themselves.
func (s *codexAppServerApiActivitySource) scanCandidate(cand codexAppServerCandidate, ctx ActivityScanContext) (bool, string) {
	if cand.socketPath == "" {
		// Daemon-assigned default socket path - not something this source
		// can dial without guessing, same limitation the hooks source has
		// for CODEX_HOME/HOME resolution. A real deployment always passes
		// an explicit --listen unix://PATH (see the container startup
		// script), so this only affects ad hoc/manual invocations.
		label := fmt.Sprintf("codex-app-server-api (pid %s, socket <daemon-assigned default>)", cand.pid)
		activityLogf(ctx.Verbose, "CLI Watcher: %s: cannot connect - socket path unresolvable", label)
		return false, label
	}

	label := fmt.Sprintf("codex-app-server-api (pid %s, socket %s)", cand.pid, cand.socketPath)

	client, wasCached, err := s.getOrDialClient(cand)
	if err != nil {
		activityLogf(ctx.Verbose, "CLI Watcher: %s: cannot connect: %v", label, err)
		return false, label
	}
	if !wasCached {
		activityLogf(ctx.Verbose, "CLI Watcher: %s: connected", label)
	}

	loadedIDs, err := client.threadLoadedList()
	if err != nil {
		// The connection may have gone stale (app-server restarted,
		// socket closed, etc.) - drop it so the next scan attempt dials
		// fresh rather than repeatedly failing on the same dead connection.
		s.dropConnection(cand.pid)
		activityLogf(ctx.Verbose, "CLI Watcher: %s: thread/loaded/list failed: %v", label, err)
		return false, label
	}
	activityLogf(ctx.Verbose, "CLI Watcher: %s: %d loaded thread(s): %v", label, len(loadedIDs), loadedIDs)

	threads := make([]codexThread, 0, len(loadedIDs))
	for _, id := range loadedIDs {
		thread, err := client.threadRead(id)
		if err != nil {
			activityLogf(ctx.Verbose, "CLI Watcher: %s: thread/read(%s) failed: %v", label, id, err)
			continue
		}
		activityLogf(ctx.Verbose, "CLI Watcher: %s: thread %s status=%s activeFlags=%v",
			label, thread.ID, thread.Status.Type, thread.Status.ActiveFlags)
		threads = append(threads, thread)
	}

	if thread, ok := findActiveThread(threads); ok {
		activityLogf(ctx.Verbose, "CLI Watcher: %s: active thread %s (activeFlags=%v)",
			label, thread.ID, thread.Status.ActiveFlags)
		s.markActiveNow(cand.pid)
		return true, label
	}

	window := ctx.ActivityWindow
	if window <= 0 {
		window = DefaultActivityWindow
	}
	if lastActiveAt, ok := s.lastActiveAt[cand.pid]; ok {
		age := time.Since(lastActiveAt)
		if age >= 0 && age < window {
			activityLogf(ctx.Verbose, "CLI Watcher: %s: no thread currently active, but last seen active %v ago (within %v activity window) - still reporting active",
				label, age.Round(time.Second), window)
			return true, label
		}
		activityLogf(ctx.Verbose, "CLI Watcher: %s: last seen active %v ago, outside %v activity window",
			label, age.Round(time.Second), window)
	}

	activityLogf(ctx.Verbose, "CLI Watcher: %s: checked %d loaded thread(s), none active", label, len(threads))
	return false, label
}

// markActiveNow records that a candidate pid was just observed with an
// active thread, for the ActivityWindow extension check above.
func (s *codexAppServerApiActivitySource) markActiveNow(pid string) {
	if s.lastActiveAt == nil {
		s.lastActiveAt = make(map[string]time.Time)
	}
	s.lastActiveAt[pid] = time.Now()
}

// getOrDialClient returns the cached connection for this candidate's pid
// (wasCached=true), dialing a fresh one if none exists yet (wasCached=false).
func (s *codexAppServerApiActivitySource) getOrDialClient(cand codexAppServerCandidate) (client *codexAppServerClient, wasCached bool, err error) {
	if client, ok := s.conns[cand.pid]; ok {
		return client, true, nil
	}

	client, err = dialCodexAppServerClient(cand.socketPath)
	if err != nil {
		return nil, false, err
	}

	if s.conns == nil {
		s.conns = make(map[string]*codexAppServerClient)
	}
	s.conns[cand.pid] = client
	return client, false, nil
}

// dropConnection closes and forgets the cached connection for a pid, if
// any.
func (s *codexAppServerApiActivitySource) dropConnection(pid string) {
	if client, ok := s.conns[pid]; ok {
		_ = client.Close()
		delete(s.conns, pid)
	}
}

// pruneStaleConnections closes and forgets any cached connection (and any
// lastActiveAt entry) whose pid is no longer among the currently
// discovered candidates (process exited or was restarted under a new
// pid) - otherwise this source would leak one open socket, and one stale
// "last active" timestamp that could wrongly extend a future unrelated
// pid's window, per codex-app-server restart for the lifetime of the
// watcher.
func (s *codexAppServerApiActivitySource) pruneStaleConnections(candidates []codexAppServerCandidate) {
	if len(s.conns) == 0 && len(s.lastActiveAt) == 0 {
		return
	}

	live := make(map[string]bool, len(candidates))
	for _, cand := range candidates {
		live[cand.pid] = true
	}

	for pid := range s.conns {
		if !live[pid] {
			s.dropConnection(pid)
		}
	}
	for pid := range s.lastActiveAt {
		if !live[pid] {
			delete(s.lastActiveAt, pid)
		}
	}
}
