package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"scrutineer/internal/egressgrant"
)

// hasEgressPolicy reports whether sj needs applyEgressPolicy: its skill has a
// policy or the operator attached a scan policy. A scan policy name counts even
// when it is not configured so applyEgressPolicy can refuse the scan.
func (d ContainerRunner) hasEgressPolicy(sj SkillJob) bool {
	return len(d.EgressPolicies[sj.Name]) > 0 || sj.EgressPolicy != ""
}

// applyEgressPolicy applies the operator's egress grants for sj to a copy of
// the runner: the skill's policy plus the scan policy the operator attached, if
// any. A job with neither gets the runner back untouched. Grants need
// --hardened: without the per-scan --internal network a scan could clear its
// proxy variables and ignore the port gate, so the scan is refused rather than
// run with grants nothing enforces. A scan policy that is no longer configured
// is refused too, so a retry never runs without the egress it was launched
// with. The returned cleanup must run after the scan and emits any denials the
// host proxy recorded.
func (d ContainerRunner) applyEgressPolicy(sj SkillJob, emit func(Event)) (ContainerRunner, func(), error) {
	noop := func() {}
	if !d.hasEgressPolicy(sj) {
		return d, noop, nil
	}
	skillGrants := d.EgressPolicies[sj.Name]
	scanGrants, ok := d.ScanEgressPolicies[sj.EgressPolicy]
	if sj.EgressPolicy != "" && !ok {
		return d, noop, fmt.Errorf("scan egress policy %q is not configured; refusing to run without the egress it was launched with", sj.EgressPolicy)
	}
	who := egressPolicyOwner(sj)
	if !d.Hardened {
		return d, noop, fmt.Errorf("egress policy for %s requires --hardened; refusing to run with unenforced grants", who)
	}
	grants := egressgrant.Merge(skillGrants, scanGrants)
	text := fmt.Sprintf("egress-policy: skill=%s grants=%s", sj.Name, egressgrant.Format(grants))
	if sj.EgressPolicy != "" {
		text = fmt.Sprintf("egress-policy: skill=%s scan_policy=%s grants=%s", sj.Name, sj.EgressPolicy, egressgrant.Format(grants))
	}
	emit(Event{Kind: KindEgress, Text: text})
	if d.usesEgressSidecar() {
		d.Egress.Grants = grants
		return d, noop, nil
	}
	if d.ProviderProxy.ContainerHost == "" {
		return d, noop, fmt.Errorf("egress policy for %s cannot start a scoped proxy because the container host endpoint is unavailable", who)
	}
	rec := &lineBuffer{}
	token := NewProxyToken()
	port, closeProxy, err := StartScopedEgressProxy(&EgressProxy{
		Allow:     d.ProviderProxy.Allow,
		Token:     token,
		APIPort:   d.ProviderProxy.APIPort,
		APIHosts:  d.ProviderProxy.APIHosts,
		HostPorts: d.Egress.HostPorts,
		Log:       slog.New(&recordingHandler{inner: forwardHandler(d.ProviderProxy.Log), rec: newWarnHandler(rec)}),
	}, grants...)
	if err != nil {
		return d, noop, fmt.Errorf("start egress policy proxy for %s: %w", who, err)
	}
	d.ProxyURL = ProxyURLForHost(token, d.ProviderProxy.ContainerHost, port)
	return d, func() {
		closeProxy()
		for _, line := range rec.lines() {
			emit(Event{Kind: KindEgress, Text: "egress-proxy: " + line})
		}
	}, nil
}

// egressPolicyOwner names what the policy belongs to for error messages. Skill
// only policies keep the original `skill "x"` wording.
func egressPolicyOwner(sj SkillJob) string {
	if sj.EgressPolicy == "" {
		return fmt.Sprintf("skill %q", sj.Name)
	}
	return fmt.Sprintf("skill %q with scan policy %q", sj.Name, sj.EgressPolicy)
}

// lineBuffer is a mutex-guarded sink for the text lines the proxy goroutines
// log. Only the scan goroutine reads it, after the proxy has closed.
type lineBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lineBuffer) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// forwardHandler returns the handler denials are also forwarded to, or a
// discarding one when the operator logger is unset.
func forwardHandler(log *slog.Logger) slog.Handler {
	if log == nil {
		return slog.NewTextHandler(io.Discard, nil)
	}
	return log.Handler()
}

func newWarnHandler(w *lineBuffer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn})
}

// recordingHandler forwards every record to the operator's logger (when one is
// configured) and also keeps WARN and above so the scan record can show what
// the per-skill proxy denied.
type recordingHandler struct {
	inner slog.Handler
	rec   slog.Handler
}

func (h *recordingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.inner.Enabled(ctx, level)
}

func (h *recordingHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.inner.Enabled(ctx, r.Level) {
		_ = h.inner.Handle(ctx, r.Clone())
	}
	if r.Level >= slog.LevelWarn {
		return h.rec.Handle(ctx, r)
	}
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{inner: h.inner.WithAttrs(attrs), rec: h.rec.WithAttrs(attrs)}
}

func (h *recordingHandler) WithGroup(name string) slog.Handler {
	return &recordingHandler{inner: h.inner.WithGroup(name), rec: h.rec.WithGroup(name)}
}
