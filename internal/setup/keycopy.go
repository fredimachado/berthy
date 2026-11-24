package setup

import (
	"bytes"
	"context"
	"errors"
	"time"

	"berthy/internal/keycopy"
	"berthy/internal/keywrap"
	"berthy/internal/scope"
)

const keyCopyTimeout = 30 * time.Second

func keyWrapScript(keyPath string) string {
	return "# key copy\nexec setpriv --reuid=berthy --regid=berthy --clear-groups /usr/local/bin/berthy agent key-wrap " + shellQuote(keyPath)
}

func keyCopyStep(ctx context.Context, s *Session) (string, string, error) {
	if s.Opts.Local {
		return StatusSkipped, "not copied: this is the server itself; run setup over SSH from your own machine to save a copy", nil
	}
	if s.Opts.KeyHome == "" {
		if s.DryRun {
			return StatusSkipped, "would not keep a copy of the encryption key: this machine has no home folder", nil
		}
		return notSaved("this machine has no home folder")
	}
	script := keyWrapScript(s.KeyFile())
	if s.DryRun {
		s.Log.Would(script)
		return StatusSkipped, "would keep a copy of the encryption key on this machine", nil
	}
	if ctx.Err() != nil {
		return notSaved("interrupted")
	}
	makeKey := s.Opts.NewWrapKey
	if makeKey == nil {
		makeKey = keywrap.Generate
	}
	wrap, err := makeKey()
	if ctx.Err() != nil {
		return notSaved("interrupted")
	}
	if err != nil {
		return notSaved("this machine could not make a wrapping key")
	}
	rctx, cancel := context.WithTimeout(ctx, keyCopyTimeout)
	res, err := s.R.Run(rctx, script, bytes.NewReader(wrap.PublicLine()))
	late := rctx.Err() != nil
	cancel()
	if ctx.Err() != nil {
		return notSaved("interrupted")
	}
	if late {
		return notSaved("the server did not answer in time")
	}
	if err != nil {
		return notSaved("the server did not hand over the key")
	}
	key, err := wrap.Unwrap(res.Stdout)
	defer clear(key)
	if errors.Is(err, keywrap.ErrUnsettled) && res.Code == 3 {
		s.KeyUnsettled = true
		return StatusSkipped, "not saved: a key rotation on the server did not finish; see the next steps", nil
	}
	if err != nil || res.Code != 0 {
		return notSaved("the server did not hand over the key")
	}
	path, created, err := keycopy.Save(s.Opts.KeyHome, scope.ID{Project: s.Opts.Project, Environment: s.Opts.Env}, key)
	if err != nil {
		return notSaved(err.Error())
	}
	s.KeyCopyPath = path
	if created {
		return StatusChanged, "saved a copy of the encryption key in " + path, nil
	}
	return StatusOK, "the copy of the encryption key is in " + path, nil
}

func notSaved(reason string) (string, string, error) {
	return StatusSkipped, "not saved: " + reason + "; run setup again once that is fixed", nil
}
