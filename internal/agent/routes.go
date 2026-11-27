package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/metrics"
	"berthy/internal/session"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

type envRoute struct {
	pattern, method, scope, kind string
	body                         reflect.Type
	optionalBody, gateDryRun     bool
	describeTarget               bool
	summary                      func(string, []string, summaryBody) string
	lane                         routeLane
	handle                       func(*routeRuntime, routeCall)
}

type routeLane uint8

const (
	ordinaryRoute routeLane = iota
	legacyReadRoute
	approvalRoute
	sessionRoute
)

type routeCall struct {
	w       http.ResponseWriter
	r       *http.Request
	actor   deploy.Actor
	scope   string
	rest    []string
	row     *envRoute
	readCtx context.Context
}

func (call routeCall) stateActor() state.Actor {
	return state.Actor{ID: call.actor.ID, Name: call.actor.Name, Remote: call.actor.Remote}
}

type routeRuntime struct {
	c         Config
	e         *engine.Engine
	collector *metrics.Collector
	d         *deploy.Controller
	tel       *telemetry.Exporter
	sched     *jobs.Scheduler
	backups   *Backups
	lim       *limits
	keep      func() func(map[string]string) bool
	tokens    *tokenSource
	gate      *approvalGate
}

type readRouteBody[T any] func() (T, error)

func jsonRoute[T any](row envRoute, endpoint func(*routeRuntime, routeCall, readRouteBody[T])) envRoute {
	row.body = reflect.TypeFor[T]()
	if endpoint != nil {
		row.handle = func(rt *routeRuntime, call routeCall) {
			endpoint(rt, call, func() (T, error) {
				var value T
				var err error
				if call.row.optionalBody {
					err = decodeOptionalBody(call.w, call.r, &value)
				} else {
					err = decodeBody(call.w, call.r, &value)
				}
				return value, err
			})
		}
	}
	return row
}

// envRoutes is read-only after init.
var envRoutes []envRoute

func init() {
	// A handler can call back into route lookup.
	envRoutes = declaredRoutes()
}

func declaredRoutes() []envRoute {
	return []envRoute{
		jsonRoute[api.ImageLookup](envRoute{pattern: "images/resolve", method: "POST", scope: "admin"}, (*routeRuntime).handleImageResolve),
		jsonRoute[imageTagsLookup](envRoute{pattern: "images/tags", method: "POST", scope: "admin"}, (*routeRuntime).handleImageTags),
		jsonRoute[deploy.Request](envRoute{pattern: "deploys", method: "POST", scope: "deploy", kind: "deploy", gateDryRun: true, summary: summarizeDeploy}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.ApplyRequest](envRoute{pattern: "apply", method: "POST", scope: "deploy", kind: "apply", gateDryRun: true, summary: fixedSummary("apply the environment settings")}, (*routeRuntime).handleApply),
		jsonRoute[deploy.Request](envRoute{pattern: "rollback", method: "POST", scope: "deploy", kind: "rollback", gateDryRun: true, summary: summarizeRollback}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "restart", method: "POST", scope: "deploy", kind: "restart", gateDryRun: true, summary: summarizeApp}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "scale", method: "POST", scope: "deploy", kind: "scale", gateDryRun: true, summary: summarizeScale}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "exec", method: "POST", scope: "admin", kind: "exec", gateDryRun: true, summary: summarizeExec}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "run", method: "POST", scope: "admin", kind: "run", gateDryRun: true, summary: summarizeApp}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "prune", method: "POST", scope: "admin", kind: "prune", gateDryRun: true, summary: summarizeKind}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "remove", method: "POST", scope: "admin", kind: "remove", gateDryRun: true, summary: summarizeRemove}, (*routeRuntime).handleAdmission),
		jsonRoute[deploy.Request](envRoute{pattern: "destroy", method: "POST", scope: "admin", kind: "destroy", gateDryRun: true, summary: fixedSummary("destroy the environment")}, (*routeRuntime).handleAdmission),
		{pattern: "operations/{id}", method: "GET", scope: "read", handle: (*routeRuntime).handleOperation},
		{pattern: "operations/{id}/output", method: "GET", scope: "admin", handle: (*routeRuntime).handleOutput},
		{pattern: "operations/{id}/events", method: "GET", scope: "read", handle: (*routeRuntime).handleEvents},
		// Failure evidence can contain the app's own output.
		{pattern: "operations/{id}/diagnostics", method: "GET", scope: "deploy", handle: (*routeRuntime).handleDiagnostics},
		jsonRoute[resolveBody](envRoute{pattern: "operations/{id}/resolve", method: "POST", scope: "admin", kind: "recovery resolve", gateDryRun: true, summary: summarizeTarget}, (*routeRuntime).handleResolve),
		// A cancel needs only the operation in the path.
		jsonRoute[dryRunBody](envRoute{pattern: "operations/{id}/cancel", method: "POST", scope: "deploy", kind: "cancel", gateDryRun: true, summary: summarizeTarget, optionalBody: true}, (*routeRuntime).handleCancel),
		{pattern: "history/{app}", method: "GET", scope: "read", handle: (*routeRuntime).handleHistory},
		{pattern: "history/{app}/diff", method: "GET", scope: "read", handle: (*routeRuntime).handleHistoryDiff},
		{pattern: "wait/{app}", method: "GET", scope: "read", handle: (*routeRuntime).handleWait},
		{pattern: "applied/{app}", method: "GET", scope: "read", handle: (*routeRuntime).handleApplied},
		{pattern: "audit", method: "GET", scope: "read", handle: (*routeRuntime).handleAudit},
		{pattern: "lock", method: "GET", scope: "read"},
		jsonRoute[lockBody](envRoute{pattern: "lock/acquire", method: "POST", scope: "deploy", kind: "lock acquire", gateDryRun: true, summary: summarizeKind}, nil),
		jsonRoute[lockBody](envRoute{pattern: "lock/release", method: "POST", scope: "deploy", kind: "lock release", gateDryRun: true, summary: summarizeKind}, nil),

		// For other methods on the collection, the expected method is POST.
		{pattern: "secrets", method: "GET", scope: "admin"},
		jsonRoute[secretBody](envRoute{pattern: "secrets", method: "POST", scope: "admin", kind: "secret set", gateDryRun: true, summary: summarizeName}, nil),
		jsonRoute[api.SecretImport](envRoute{pattern: "secrets/import", method: "POST", scope: "admin", kind: "secret import", gateDryRun: true, summary: summarizeImport}, nil),
		jsonRoute[api.SecretGenerate](envRoute{pattern: "secrets/generate", method: "POST", scope: "admin", kind: "secret generate", gateDryRun: true, summary: summarizeName}, nil),
		jsonRoute[api.SecretsGenerateDeclared](envRoute{pattern: "secrets/generate-declared", method: "POST", scope: "admin", kind: "secret generate declared", gateDryRun: true, summary: fixedSummary("generate the declared secrets")}, nil),
		// DELETE applies even when the name matches one of the literal POST paths.
		jsonRoute[secretBody](envRoute{pattern: "secrets/{name}", method: "DELETE", scope: "admin", kind: "secret delete", gateDryRun: true, summary: summarizeTarget}, nil),
		jsonRoute[deploy.Request](envRoute{pattern: "ingress/apply", method: "POST", scope: "deploy", kind: "ingress apply", gateDryRun: true, summary: summarizeKind}, (*routeRuntime).handleIngressApply),
		{pattern: "ingress/status", method: "GET", scope: "read", handle: (*routeRuntime).handleIngressStatus},
		{pattern: "checks", method: "GET", scope: "read"},
		{pattern: "jobs", method: "GET", scope: "read", handle: (*routeRuntime).handleJobs},
		{pattern: "jobs/{name}/runs", method: "GET", scope: "read", handle: (*routeRuntime).handleJobRuns},
		{pattern: "jobs/{name}/run", method: "POST", scope: "deploy", kind: "job run", summary: summarizeTarget, describeTarget: true, handle: (*routeRuntime).handleJobRun},
		{pattern: "jobs/{name}/cancel", method: "POST", scope: "deploy", kind: "job cancel", summary: summarizeTarget, describeTarget: true, handle: (*routeRuntime).handleJobCancel},
		// Failed runs can disclose output unavailable to a read token.
		{pattern: "jobs/{name}/logs", method: "GET", scope: "deploy", handle: (*routeRuntime).handleJobLogs},
		{pattern: "backups", method: "GET", scope: "read", handle: (*routeRuntime).handleBackups},
		{pattern: "backups/{name}/runs", method: "GET", scope: "read", handle: (*routeRuntime).handleBackupRuns},
		{pattern: "backups/{name}/run", method: "POST", scope: "deploy", kind: "backup run", summary: summarizeTarget, describeTarget: true, handle: (*routeRuntime).handleBackupRun},
		{pattern: "state-backup", method: "POST", scope: "admin", kind: "state backup run", summary: summarizeKind, handle: (*routeRuntime).handleStateBackupRun},
		{pattern: "state-backup/runs", method: "GET", scope: "read", handle: (*routeRuntime).handleStateBackupRuns},
		jsonRoute[restoreBody](envRoute{pattern: "restore", method: "POST", scope: "admin", kind: "restore", gateDryRun: true, summary: summarizeRestore}, (*routeRuntime).handleRestore),
		jsonRoute[dryRunBody](envRoute{pattern: "key/rotate", method: "POST", scope: "admin"}, (*routeRuntime).serveKeyRotate),
		jsonRoute[session.Request](envRoute{pattern: "sessions", method: "POST", scope: "admin", lane: sessionRoute}, (*routeRuntime).serveSession),
		{pattern: "alerts/test", method: "POST", scope: "deploy", kind: "alerts test", summary: summarizeKind},

		// For other methods on the collection, the expected method is POST.
		{pattern: "tokens", method: "GET", scope: "admin", handle: (*routeRuntime).handleTokens},
		jsonRoute[createTokenBody](envRoute{pattern: "tokens", method: "POST", scope: "admin"}, (*routeRuntime).handleTokenCreate),
		jsonRoute[dryRunBody](envRoute{pattern: "tokens/{name}", method: "DELETE", scope: "admin", optionalBody: true}, (*routeRuntime).handleTokenRevoke),

		{pattern: "approvals", method: "GET", scope: "read", lane: approvalRoute, handle: (*routeRuntime).handleApprovals},
		{pattern: "approvals/{id}/approve", method: "POST", scope: "admin", lane: approvalRoute, handle: (*routeRuntime).handleApprove},
		jsonRoute[rejectBody](envRoute{pattern: "approvals/{id}/reject", method: "POST", scope: "admin", lane: approvalRoute, optionalBody: true}, (*routeRuntime).handleReject),

		{pattern: "agent", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleAgent},
		{pattern: "apps", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleApps},
		{pattern: "apps/{app}", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleApp},
		{pattern: "apps/{app}/logs", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleAppLogs},
		{pattern: "df", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleDF},
		{pattern: "doctor", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleDoctor},
		{pattern: "stats", method: "GET", scope: "read", lane: legacyReadRoute, handle: (*routeRuntime).handleStats},
	}
}

// findEnvRoute prefers an exact method match, falling back to the last matching path for wrong methods.
func findEnvRoute(rest []string, method string, changesOnly bool) *envRoute {
	var fallback *envRoute
	for i := range envRoutes {
		row := &envRoutes[i]
		if changesOnly && row.kind == "" || !matchesRoute(row.pattern, rest) {
			continue
		}
		if row.method == method {
			return row
		}
		fallback = row
	}
	return fallback
}

func matchesRoute(pattern string, rest []string) bool {
	i := 0
	for segment := range strings.SplitSeq(pattern, "/") {
		if i == len(rest) {
			return false
		}
		if segment != rest[i] && !(strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")) {
			return false
		}
		i++
	}
	return i == len(rest)
}

type restoreBody struct {
	Key string `json:"key"`
	// Into is the new volume a volume backup fills. App, Service or Role, Command and Timeout name the command a
	// command backup is fed to, and where it runs.
	Into      string   `json:"into"`
	App       string   `json:"app"`
	Service   string   `json:"service"`
	Role      string   `json:"role"`
	Command   []string `json:"command"`
	Timeout   string   `json:"timeout"`
	DryRun    bool     `json:"dry_run"`
	RequestID string   `json:"request_id"`
}

type resolveBody struct {
	Reason string `json:"reason"`
	DryRun bool   `json:"dry_run"`
}

type lockBody struct {
	Message string `json:"message"`
	DryRun  bool   `json:"dry_run"`
}

type secretBody struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Registry string `json:"registry"`
	DryRun   bool   `json:"dry_run"`
}

type dryRunBody struct {
	DryRun bool `json:"dry_run"`
}

type createTokenBody struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Agent   bool   `json:"agent"`
	Expires string `json:"expires"`
	DryRun  bool   `json:"dry_run"`
}

type rejectBody struct {
	Reason string `json:"reason"`
}

type summaryBody struct {
	App           string   `json:"app"`
	Version       string   `json:"version"`
	KeepVersion   bool     `json:"keep_version"`
	KeepDigest    string   `json:"keep_digest"`
	Role          string   `json:"role"`
	Replicas      *int     `json:"replicas"`
	TargetRelease int64    `json:"target_release"`
	Name          string   `json:"name"`
	Key           string   `json:"key"`
	Into          string   `json:"into"`
	Volumes       bool     `json:"volumes"`
	Service       string   `json:"service"`
	Command       []string `json:"command"`
	// Secrets is counted, never read: an import's entries hold values.
	Secrets []json.RawMessage `json:"secrets"`
}

// summarize is the one line that says what a held request does. It carries no secret value.
func summarize(rest []string, method string, body []byte) string {
	row := findEnvRoute(rest, method, true)
	if row == nil {
		return "change"
	}
	var b summaryBody
	_ = json.Unmarshal(body, &b)
	return row.summary(row.kind, rest, b)
}

func summarizeKind(kind string, _ []string, _ summaryBody) string {
	return kind
}

func summarizeTarget(kind string, rest []string, _ summaryBody) string {
	return kind + " " + cleanText(rest[1], 80)
}

func summarizeName(kind string, _ []string, b summaryBody) string {
	return kind + " " + cleanText(b.Name, 80)
}

func summarizeApp(kind string, _ []string, b summaryBody) string {
	return kind + " " + b.appWithRole()
}

func (b summaryBody) appWithRole() string {
	app := cleanText(b.App, 80)
	if b.Role != "" {
		app += "/" + cleanText(b.Role, 40)
	}
	return app
}

func summarizeDeploy(_ string, _ []string, b summaryBody) string {
	if b.KeepVersion && b.Version == "" {
		if b.KeepDigest != "" {
			return "deploy " + cleanText(b.App, 80) + " (keeps " + cleanText(deploy.ShortDigest(b.KeepDigest), 80) + ")"
		}
		return "deploy " + cleanText(b.App, 80) + " (keeps the current version)"
	}
	return strings.TrimSpace("deploy " + cleanText(b.App, 80) + " " + cleanText(deploy.ShortDigest(b.Version), 80))
}

func summarizeRollback(_ string, _ []string, b summaryBody) string {
	if b.TargetRelease > 0 {
		return "rollback " + cleanText(b.App, 80) + " to release " + strconv.FormatInt(b.TargetRelease, 10)
	}
	return "rollback " + cleanText(b.App, 80)
}

func summarizeScale(_ string, _ []string, b summaryBody) string {
	if b.Replicas != nil {
		return "scale " + b.appWithRole() + " to " + strconv.Itoa(*b.Replicas)
	}
	return "scale " + b.appWithRole()
}

func summarizeExec(_ string, _ []string, b summaryBody) string {
	if b.Service != "" {
		return "exec " + cleanText(b.App, 80) + "/" + cleanText(b.Service, 40)
	}
	return "exec " + b.appWithRole()
}

func summarizeRemove(_ string, _ []string, b summaryBody) string {
	if b.Volumes {
		return "remove " + cleanText(b.App, 80) + " and its volumes"
	}
	return "remove " + cleanText(b.App, 80)
}

func summarizeRestore(_ string, _ []string, b summaryBody) string {
	if len(b.Command) > 0 {
		// Command arguments may hold a secret.
		service := ""
		if b.Service != "" {
			service = "/" + cleanText(b.Service, 40)
		}
		return "restore " + cleanText(b.Key, 100) + " into a command on " + b.appWithRole() + service
	}
	return "restore " + cleanText(b.Key, 100) + " into " + cleanText(b.Into, 60)
}

func summarizeImport(_ string, _ []string, b summaryBody) string {
	if len(b.Secrets) == 1 {
		return "secret import 1 secret"
	}
	return "secret import " + strconv.Itoa(len(b.Secrets)) + " secrets"
}

func fixedSummary(text string) func(string, []string, summaryBody) string {
	return func(string, []string, summaryBody) string { return text }
}
