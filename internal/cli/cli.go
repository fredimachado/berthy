package cli

import (
	"berthy/internal/agent"
	"berthy/internal/api"
	"berthy/internal/engine"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Config struct {
	Environments map[string]struct {
		URL      string `json:"url"`
		TokenEnv string `json:"token_env"`
	} `json:"environments"`
}

func Run(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, "usage: berthy <agent|token|ps|describe|df|doctor|agent-info>")
		return 2
	}
	cmd := args[0]
	args = args[1:]
	if cmd == "agent" && len(args) == 3 && args[0] == "token" && args[1] == "-e" {
		if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(args[2]) {
			fmt.Fprintln(errout, "invalid environment")
			return 2
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		token := "bty." + args[2] + "." + hex.EncodeToString(b)
		fmt.Fprintf(out, "token: %s\nsha256: %s\n", token, agent.Digest(token))
		return 0
	}
	var config, env string
	var jsonOutput bool
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-e":
			if i+1 == len(args) {
				fmt.Fprintln(errout, "missing flag value")
				return 2
			}
			i++
			if args[i-1] == "--config" {
				config = args[i]
			} else {
				env = args[i]
			}
		case "--json":
			jsonOutput = true
		default:
			positional = append(positional, args[i])
		}
	}
	if cmd == "agent" {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(errout, "agent requires Linux")
			return 1
		}
		if config == "" {
			fmt.Fprintln(errout, "agent requires --config")
			return 2
		}
		c, err := agent.Load(config)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		server := http.Server{Addr: c.Listen, Handler: agent.Handler(c, engine.New(c.DockerSocket)), ReadHeaderTimeout: 5 * time.Second}
		if err = server.ListenAndServe(); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	}
	if env == "" || config == "" {
		fmt.Fprintln(errout, "remote commands require -e and --config")
		return 2
	}
	b, err := os.ReadFile(config)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	var c Config
	if err = json.Unmarshal(b, &c); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	target, ok := c.Environments[env]
	if !ok || target.TokenEnv == "" {
		fmt.Fprintln(errout, "unknown environment or missing token_env")
		return 1
	}
	token := os.Getenv(target.TokenEnv)
	if token == "" {
		fmt.Fprintln(errout, "token environment variable is empty")
		return 1
	}
	u, err := url.Parse(target.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		fmt.Fprintln(errout, "invalid agent URL")
		return 1
	}
	var endpoint string
	switch cmd {
	case "ps":
		endpoint = "apps"
	case "describe":
		if len(positional) != 1 {
			fmt.Fprintln(errout, "describe requires an app")
			return 2
		}
		endpoint = "apps/" + url.PathEscape(positional[0])
	case "df", "doctor":
		endpoint = cmd
	case "agent-info":
		endpoint = "agent"
	default:
		fmt.Fprintln(errout, "unknown command")
		return 2
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(env) + "/" + endpoint
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if resp.StatusCode != 200 {
		fmt.Fprintf(errout, "agent: %s: %s", resp.Status, body)
		return 1
	}
	if jsonOutput {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
	} else {
		var v any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		printText(out, v, "")
	}

	if cmd == "doctor" {
		var d struct {
			Checks []api.Check `json:"checks"`
		}
		if json.Unmarshal(body, &d) != nil {
			return 1
		}
		for _, ch := range d.Checks {
			if ch.Critical && !ch.OK {
				return 1
			}
		}
	}
	return 0
}
func printText(w io.Writer, v any, indent string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := x[k]
			fmt.Fprintf(w, "%s%s: ", indent, k)
			switch v.(type) {
			case map[string]any, []any:
				fmt.Fprintln(w)
				printText(w, v, indent+"  ")
			default:
				fmt.Fprintln(w, v)
			}
		}
	case []any:
		for _, v := range x {
			printText(w, v, indent)
			fmt.Fprintln(w)
		}
	default:
		fmt.Fprintln(w, x)
	}
}
