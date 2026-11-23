package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/manifest"
)

type guideResult int

const (
	guideDone guideResult = iota
	guideHeld
	guideFailed
)

func runSecretSetMissing(f commandFlags, target *agentclient.Target, dir string, out, errout io.Writer) int {
	read, ok := hiddenInput()
	if !ok {
		fmt.Fprintln(errout, "secret set without a NAME asks at a terminal: pipe a value to berthy secret set NAME, or use berthy secret import")
		return 2
	}
	m, err := manifest.Load(dir, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if !manifestMatchesTarget(errout, dir, m.Project, target.Project) {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	unmet, result := guideSecrets(ctx, agentclient.NewHTTPClient(), target, m, dir, f.env, read, out, errout)
	if result == guideHeld {
		return exitHeld
	}
	if result == guideFailed || len(unmet) != 0 {
		return 1
	}
	return 0
}

func guideSecrets(ctx context.Context, client *http.Client, target *agentclient.Target, m *manifest.Manifest, dir, env string, read func() ([]byte, error), out, errout io.Writer) ([]unmetNeed, guideResult) {
	declared, call := generateDeclared(ctx, client, target, m, dir, false, out, errout, false)
	if call == declaredHeld {
		return nil, guideHeld
	}
	if call == declaredFailed {
		return nil, guideFailed
	}
	printDeclared(out, declared, false)
	refused := map[string]api.DeclaredSecretOutcome{}
	for _, s := range declared.Secrets {
		if s.Outcome == api.DeclaredRefuse {
			refused[s.Name] = s
		}
	}
	endpoint := agentURL(target.Base, target.Prefix, "secrets")
	held, ok := storedSecrets(ctx, client, endpoint, target.Token, errout)
	if !ok {
		return nil, guideFailed
	}
	needs := unmetSecrets(m, held)
	if len(needs) == 0 {
		fmt.Fprintln(out, "every secret the manifest needs is stored")
		return needs, guideDone
	}
	var remaining []unmetNeed
	for _, n := range needs {
		if ctx.Err() != nil {
			return remaining, guideFailed
		}
		name := terminalSafe(n.Name)
		fmt.Fprintf(errout, "%s is needed by %s\n", name, terminalSafe(strings.Join(n.NeededBy, ", ")))
		switch n.Kind {
		case unmetShared:
			fmt.Fprintf(errout, "%s is the password_secret of %s: give each registry its own secret name\n", name, safeRegistryList(n.Registries))
			remaining = append(remaining, n)
			continue
		case unmetMixed:
			fmt.Fprintf(errout, "%s is needed by %s: give them separate secret names\n", name, mixedReaders(n))
			remaining = append(remaining, n)
			continue
		case unmetNotPlain:
			fmt.Fprintf(errout, "%s is stored as the password of %s: remove it with berthy secret rm %s, then set it\n", name, terminalSafe(n.Stored), name)
			remaining = append(remaining, n)
			continue
		case unmetRegistry:
			if n.Stored == "" {
				fmt.Fprintf(errout, "%s is stored without the registry %s: enter it again and it is stored for %s\n", name, terminalSafe(n.Registry), terminalSafe(n.Registry))
			} else {
				fmt.Fprintf(errout, "%s is stored for the registry %s: enter the password for %s and it replaces it\n", name, terminalSafe(n.Stored), terminalSafe(n.Registry))
			}
		}
		if s, ok := refused[n.Name]; ok {
			if s.Volume == "" {
				fmt.Fprintf(errout, "app %s was deployed here: enter the value it runs with\n", terminalSafe(s.App))
			} else {
				fmt.Fprintf(errout, "the volume %s of app %s exists: enter the value it was created with\n", terminalSafe(s.Volume), terminalSafe(s.App))
			}
		}
		g := guideFor(m, env, n.Name, n.Registry)
		for i := range g.intro {
			g.intro[i] = terminalSafe(g.intro[i])
		}
		if check := g.check; check != nil {
			g.check = func(_ context.Context, value string) checkResult {
				checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				res := check(checkCtx, value)
				for i := range res.notes {
					res.notes[i] = terminalSafe(res.notes[i])
				}
				res.refusal = terminalSafe(res.refusal)
				return res
			}
		}
		for {
			if ctx.Err() != nil {
				return remaining, guideFailed
			}
			value, result := askSecret(g, fmt.Sprintf("Value of %s (hidden, Enter skips): ", name), read, errout)
			if ctx.Err() != nil {
				return remaining, guideFailed
			}
			if result == askFailed {
				return remaining, guideFailed
			}
			if result == askRefused {
				continue
			}
			if result == askEmpty {
				fmt.Fprintf(out, "skipped %s\n", name)
				remaining = append(remaining, n)
				break
			}
			payload, _ := json.Marshal(map[string]string{"name": n.Name, "value": value, "registry": n.Registry})
			status, data, ok := askAgent(ctx, client, endpoint, target.Token, http.MethodPost, payload, errout)
			if !ok {
				return remaining, guideFailed
			}
			if status == http.StatusAccepted {
				if code, held := reportHeld(data, out, errout, false); held && code == exitHeld {
					return remaining, guideHeld
				}
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if status != http.StatusOK {
				fmt.Fprintln(errout, agentclient.Message(status, data))
				return remaining, guideFailed
			}
			var reply secretSetReply
			if json.Unmarshal(data, &reply) != nil || !reply.Ok || reply.Registry != n.Registry {
				fmt.Fprintln(errout, "invalid agent response")
				return remaining, guideFailed
			}
			if n.Registry != "" {
				printRegistrySet(out, reply)
			} else {
				fmt.Fprintf(out, "stored %s\n", name)
			}
			break
		}
	}
	return remaining, guideDone
}
