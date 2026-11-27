package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/strictjson"
)

const removalTime = "20060102T150405Z"

func removalRecordPath(id scope.ID) string { return agentConfigDir + "/remove-" + id.Slug() + ".json" }

func configDigest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func validHexID(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil && strings.ToLower(s) == s
}

// Read only a private regular file with an expected owner and one link. Exit 3 is reserved for verified absence.
func readRemovalFile(ctx context.Context, r Runner, name, file, owner string) ([]byte, error) {
	script := "# read " + name + "\nset -eu\n" + removalStat
	script += "file=" + shellQuote(file) + "\nstat_entry \"$file\"\n"
	script += "[ \"$entry\" != absent ] || exit 3\nowner=" + owner + "\n"
	script += "case \"$entry\" in *\" $owner 600 1 regular file\") ;; *) exit 1;; esac\ncat -- \"$file\"\n"
	res, err := r.Run(ctx, script, nil)
	if err == nil && len(res.Stderr) == 0 {
		if res.Code == 3 && len(res.Stdout) == 0 {
			return nil, nil
		}
		if res.Code == 0 && len(res.Stdout) != 0 {
			return res.Stdout, nil
		}
	}
	return nil, fmt.Errorf("could not read %s", name)
}

func readRemovalRecord(ctx context.Context, r Runner, id scope.ID, purge bool) (*removal, error) {
	raw, err := readRemovalFile(ctx, r, "removal record", removalRecordPath(id), "0")
	if err != nil || raw == nil {
		return nil, err
	}
	var rec removal
	if err := decodeRemovalRecord(raw, &rec); err != nil || rec.ID != id {
		return nil, errors.New("invalid removal record")
	}
	if purge != (rec.Archive == "") {
		return nil, errors.New("this removal was started with a different purge choice")
	}
	return &rec, nil
}

func decodeRemovalRecord(raw []byte, rec *removal) error {
	invalid := errors.New("invalid removal record")
	if err := strictjson.Unmarshal(raw, rec); err != nil {
		return invalid
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 13 {
		return invalid
	}
	for name, value := range fields {
		if bytes.Equal(value, []byte("null")) && name != "NextIdentity" && name != "PrevIdentity" {
			return invalid
		}
	}
	for _, name := range []string{"StateIdentity", "KeyIdentity", "NextIdentity", "PrevIdentity"} {
		if bytes.Equal(fields[name], []byte("null")) {
			continue
		}
		var identity struct{ Device, Inode *uint64 }
		if err := json.Unmarshal(fields[name], &identity); err != nil || identity.Device == nil || identity.Inode == nil || *identity.Inode == 0 {
			return invalid
		}
	}
	if !removalPathOK(rec.StateDir) || !removalPathOK(rec.KeyFile) ||
		!validHexID(rec.OriginalSHA) || !validHexID(rec.CandidateSHA) || rec.OriginalSHA == rec.CandidateSHA ||
		(rec.Network != "" && !manifest.ValidNetwork(rec.Network)) ||
		(rec.NetworkID != "" && (rec.Network == "" || !swarmNetworkID.MatchString(rec.NetworkID))) {
		return invalid
	}
	if rec.Archive != "" {
		timestamp, ok := strings.CutPrefix(rec.Archive, rec.StateDir+".removed-")
		if _, err := time.Parse(removalTime, timestamp); !ok || err != nil {
			return invalid
		}
	}
	return nil
}

func writeRemovalRecord(ctx context.Context, r Runner, rec removal) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return errors.New("could not encode the removal record")
	}
	file := removalRecordPath(rec.ID)
	script := "# write removal record\nset -eu\numask 077\n" +
		"tmp=$(mktemp " + shellQuote(file+".XXXXXX") + ")\n" +
		"trap 'rm -f -- \"$tmp\"' EXIT\ncat > \"$tmp\"\n" +
		fmt.Sprintf("[ \"$(wc -c < \"$tmp\")\" -eq %d ]\n", len(raw)) +
		"sync -- \"$tmp\"\nmv -fT -- \"$tmp\" " + shellQuote(file) + "\n" +
		"sync -- " + shellQuote(agentConfigDir) + "\n"
	res, err := r.Run(ctx, script, bytes.NewReader(raw))
	if err != nil || res.Code != 0 {
		return errors.New("could not write the removal record")
	}
	return nil
}
