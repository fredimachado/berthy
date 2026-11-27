package setup

import (
	"context"
	"errors"
	"fmt"

	"berthy/internal/agent"
	"berthy/internal/scope"
	"berthy/internal/strictjson"
)

func withoutHosted(raw []byte, id scope.ID) ([]byte, agent.Hosted, error) {
	var original agent.File
	if err := strictjson.Unmarshal(raw, &original); err != nil {
		return nil, agent.Hosted{}, fmt.Errorf("%s cannot be read as an unambiguous agent configuration", configPath)
	}
	if err := original.Validate(); err != nil {
		return nil, agent.Hosted{}, fmt.Errorf("%s is not a valid agent configuration", configPath)
	}
	for i, target := range original.Environments {
		if target.Project != id.Project || target.Environment != id.Environment {
			continue
		}
		if len(original.Environments) == 1 {
			return nil, agent.Hosted{}, fmt.Errorf("%s is the last hosted environment and cannot be removed", id)
		}
		ranges, err := environmentRanges(raw)
		if err != nil || len(ranges) != len(original.Environments) {
			return nil, agent.Hosted{}, errors.New("cannot locate the hosted environment entries")
		}

		start, end := ranges[i][0], ranges[i][1]
		if i+1 < len(ranges) {
			end = ranges[i+1][0]
		} else {
			start = ranges[i-1][1]
		}
		candidate := make([]byte, 0, len(raw)-(end-start))
		candidate = append(candidate, raw[:start]...)
		candidate = append(candidate, raw[end:]...)
		if _, err := decodeAgentConfig(candidate); err != nil {
			return nil, agent.Hosted{}, errors.New("the agent configuration after removal would be invalid")
		}
		return candidate, target, nil
	}
	return nil, agent.Hosted{}, fmt.Errorf("%s is not hosted on this server", id)
}

func requireNoAppliedApps(ctx context.Context, r Runner, id scope.ID, stateDir string) error {
	script := "# check applied apps\nexec setpriv --reuid=berthy --regid=berthy --clear-groups /usr/local/bin/berthy agent has-applied-apps " +
		shellQuote(id.Project) + " " + shellQuote(id.Environment) + " " + shellQuote(stateDir)
	res, err := r.Run(ctx, script, nil)
	if err == nil && ctx.Err() == nil {
		switch res.Code {
		case 0:
			switch string(res.Stdout) {
			case "empty\n":
				return nil
			case "applied\n":
				return fmt.Errorf("%s still has applied apps. Run berthy destroy --project %s -e %s --confirm %s --allow-recreate first",
					id, id.Project, id.Environment, id)
			}
		case 2, 127:
			return fmt.Errorf("could not verify applied apps for %s. If the installed agent is older, upgrade the server with berthy setup", id)
		}
	}
	return fmt.Errorf("could not verify applied apps for %s", id)
}
