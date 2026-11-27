package setup

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"berthy/internal/agent"
)

func removalNetworkJSON(id, containers string) string {
	return fmt.Sprintf(`{"Id":%q,"Name":"alpha-dev","Scope":"swarm","Driver":"overlay","Attachable":true,"Ingress":false,"Containers":%s}`, id, containers) + "\n"
}

func TestRemovalOnlyDeletesTheVerifiedUnusedNetwork(t *testing.T) {
	const id = removalTestNetworkID
	const replacement = "bcxk9hlk7pka26iwkly94laa0"
	const service = "cdl87ktajd639m3ilh3hvn53d"
	for _, tt := range []struct {
		name, want, text string
		rmCalls          int
	}{
		{"vacant", StatusChanged, "removed network", 1},
		{"internal endpoint only", StatusChanged, "removed network", 1},
		{"active attachment", StatusSkipped, "kept", 0},
		{"stopped attachment", StatusSkipped, "kept", 0},
		{"service reference", StatusSkipped, "kept", 0},
		{"absent", StatusOK, "already absent", 0},
		{"replacement", StatusFailed, "changed", 0},
		{"wrong kind", StatusFailed, "expected attachable swarm overlay", 0},
		{"ambiguous listing", StatusFailed, "identify", 0},
		{"daemon error", StatusFailed, "verify", 0},
		{"inventory warning", StatusFailed, "container attachments", 0},
		{"inactive Docker", StatusFailed, "could not be verified as running", 0},
		{"race attached", StatusSkipped, "kept", 1},
		{"race removed", StatusOK, "already absent", 1},
		{"refused", StatusFailed, "could not remove", 1},
		{"preview vacant", StatusWouldChange, "would remove", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rmCalls := 0
			r := funcRunner{run: func(script string, stdin io.Reader) Result {
				if stdin != nil {
					t.Fatalf("unexpected input to %q", script)
				}
				if strings.HasPrefix(script, "# verify removal Docker service\n") {
					if tt.name == "inactive Docker" {
						return Result{Code: 1}
					}
					if !strings.Contains(script, "/proc/net/unix") {
						t.Fatalf("custom socket must be verified without connecting: %q", script)
					}
					return Result{}
				}
				if tt.name == "inactive Docker" || !strings.Contains(script, "docker --host 'unix:///custom/docker.sock'") {
					t.Fatalf("unexpected daemon request: %q", script)
				}
				out := ""
				switch {
				case strings.Contains(script, "network ls"):
					switch {
					case tt.name == "absent" || tt.name == "race removed" && rmCalls != 0:
					case tt.name == "replacement":
						out = replacement + " alpha-dev\n"
					case tt.name == "ambiguous listing":
						out = id + " alpha-dev\n" + replacement + " alpha-dev\n"
					case tt.name == "daemon error":
						return Result{Code: 1, Stderr: []byte("private payload")}
					default:
						out = id + " alpha-dev\n"
					}
				case strings.Contains(script, "network inspect"):
					out = removalNetworkJSON(id, "{}")
					switch {
					case tt.name == "wrong kind":
						out = strings.Replace(out, "overlay", "bridge", 1)
					case tt.name == "internal endpoint only":
						out = removalNetworkJSON(id, `{"lb-alpha-dev":{"Name":"alpha-dev-endpoint"}}`)
					case tt.name == "active attachment" || tt.name == "race attached" && rmCalls != 0:
						out = removalNetworkJSON(id, `{"container":{"Name":"target"}}`)
					}
				case strings.Contains(script, "container ls -aq"):
					if !strings.Contains(script, "'network="+id+"'") {
						t.Fatalf("container check missing exact network ID: %q", script)
					}
					if tt.name == "stopped attachment" {
						out = strings.Repeat("b", 64) + "\n"
					}
					if tt.name == "inventory warning" {
						return Result{Stderr: []byte("private payload")}
					}
				case strings.Contains(script, "service ls -q"):
					out = service[:12] + "\n"
				case strings.Contains(script, "service inspect"):
					out = service + "\n"
					if tt.name == "service reference" {
						out = service + " " + id + "\n"
					}
				case strings.Contains(script, "network rm"):
					rmCalls++
					if !strings.HasSuffix(script, "network rm "+shellQuote(id)) {
						t.Fatalf("removal did not use only the verified network ID: %q", script)
					}
					if tt.name == "race attached" || tt.name == "race removed" || tt.name == "refused" {
						return Result{Code: 1, Stderr: []byte("private payload")}
					}
					out = id + "\n"
				default:
					t.Fatalf("unexpected Docker command: %q", script)
				}
				return Result{Stdout: []byte(out)}
			}}
			s := &Session{R: r, DryRun: tt.name == "preview vacant", existing: &agent.File{DockerSocket: "/custom/docker.sock"},
				removal: &removal{Activated: true, Network: "alpha-dev", NetworkID: id}}
			status, detail, err := removeNetworkStep(context.Background(), s)
			if err != nil {
				status, detail = StatusFailed, err.Error()
			}
			if status != tt.want || !strings.Contains(detail, tt.text) || strings.Contains(detail, "private payload") || rmCalls != tt.rmCalls {
				t.Fatalf("result = %q %q, remove commands = %d, want %q containing %q and %d commands", status, detail, rmCalls, tt.want, tt.text, tt.rmCalls)
			}
		})
	}
}
