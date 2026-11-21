package agent

import (
	"errors"
	"testing"

	"berthy/internal/scope"
)

func TestParseFileReadsTwoEnvironmentsOfOneProjectAndOneOfAnother(t *testing.T) {
	d1, d2, d3 := Digest("shop-prod"), Digest("shop-dev"), Digest("blog-prod")
	b := []byte(`{
		"listen": "127.0.0.1:9999",
		"docker_socket": "/var/run/docker.sock",
		"environments": [
			{"project":"shop","environment":"prod","state_dir":"/var/lib/berthy/shop-prod","key_file":"/etc/berthy/shop-prod.key","protected":true,"network":"shop-prod","tokens":[{"digest":"` + d1 + `","scope":"admin"}]},
			{"project":"shop","environment":"dev","state_dir":"/var/lib/berthy/shop-dev","key_file":"/etc/berthy/shop-dev.key","network":"shop-dev","tokens":[{"digest":"` + d2 + `","scope":"admin"}]},
			{"project":"blog","environment":"prod","state_dir":"/var/lib/berthy/blog-prod","key_file":"/etc/berthy/blog-prod.key","network":"blog-prod","tokens":[{"digest":"` + d3 + `","scope":"admin"}]}
		]
	}`)
	f, err := ParseFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Listen != "127.0.0.1:9999" || f.DockerSocket != "/var/run/docker.sock" || len(f.Environments) != 3 {
		t.Fatalf("file %+v", f)
	}
	shopProd, shopDev, blogProd := f.Environments[0], f.Environments[1], f.Environments[2]
	if shopProd.Project != "shop" || shopProd.Environment != "prod" || !shopProd.Protected || shopProd.StateDir != "/var/lib/berthy/shop-prod" || shopProd.KeyFile != "/etc/berthy/shop-prod.key" || shopProd.Network != "shop-prod" || len(shopProd.Tokens) != 1 || shopProd.Tokens[0].Digest != d1 {
		t.Errorf("shop/prod %+v", shopProd)
	}
	if shopDev.Project != "shop" || shopDev.Environment != "dev" || shopDev.Protected || shopDev.Network != "shop-dev" {
		t.Errorf("shop/dev %+v", shopDev)
	}
	if blogProd.Project != "blog" || blogProd.Environment != "prod" || blogProd.Network != "blog-prod" {
		t.Errorf("blog/prod %+v", blogProd)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid file refused: %v", err)
	}
}

func TestParseFileRefusesTheLayoutOfAnEarlierRelease(t *testing.T) {
	b := []byte(`{"environment":"lab","listen":"127.0.0.1:9999","docker_socket":"/var/run/docker.sock","tokens":[{"digest":"` + Digest("x") + `","scope":"read"}]}`)
	_, err := ParseFile(b)
	if !errors.Is(err, ErrOldLayout) {
		t.Fatalf("got %v", err)
	}
}

func TestFileValidateRefuses(t *testing.T) {
	shopProd := testHosted("shop", "prod", "shop-prod")
	shopDev := testHosted("shop", "dev", "shop-dev")
	if err := testFile(shopProd, shopDev).Validate(); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	dupPair := testHosted("shop", "prod", "shop-prod-2")
	dupState := shopDev
	dupState.StateDir = shopProd.StateDir
	dupKey := shopDev
	dupKey.KeyFile = shopProd.KeyFile
	dupNet := shopDev
	dupNet.Network = shopProd.Network
	dupDigest := shopDev
	dupDigest.Tokens = []Token{{Digest: shopProd.Tokens[0].Digest, Scope: "admin"}}
	badProject := testHosted("Shop", "prod", "bad-project")
	noState := shopProd
	noState.StateDir = ""
	noKey := shopProd
	noKey.KeyFile = ""
	leadingHyphen := testHosted("shop", "-prod", "leading-hyphen")
	for _, tc := range []struct {
		name string
		file File
	}{
		{"the same pair twice", testFile(shopProd, dupPair)},
		{"shop/prod-eu with shop-prod/eu", testFile(testHosted("shop", "prod-eu", "a"), testHosted("shop-prod", "eu", "b"))},
		{"a state directory twice", testFile(shopProd, dupState)},
		{"a key file twice", testFile(shopProd, dupKey)},
		{"a network twice", testFile(shopProd, dupNet)},
		{"a token digest in two environments", testFile(shopProd, dupDigest)},
		{"an invalid project", testFile(badProject)},
		{"no environment", testFile()},
		{"an environment without a state directory", testFile(noState)},
		{"an environment without a key file", testFile(noKey)},
		{"an environment name with a leading hyphen", testFile(leadingHyphen)},
	} {
		if err := tc.file.Validate(); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
}

func TestConfigsCarryTheSharedSettings(t *testing.T) {
	f := File{
		Listen:           "127.0.0.1:9999",
		DockerSocket:     "/var/run/docker.sock",
		CloudflareAPIURL: "http://127.0.0.1:1",
		TelegramAPIURL:   "http://127.0.0.1:2",
		ComposeCommand:   []string{"/opt/compose"},
		ApprovalTTL:      "20m",
		Environments: []Hosted{
			{Project: "shop", Environment: "prod", StateDir: "/s/prod", KeyFile: "/k/prod", Protected: true, Network: "shop-prod", Tokens: []Token{{Digest: Digest("a"), Scope: "admin"}}},
			{Project: "blog", Environment: "dev", StateDir: "/s/dev", KeyFile: "/k/dev", Tokens: []Token{{Digest: Digest("b"), Scope: "read"}}},
		},
	}
	cfgs := f.Configs()
	if len(cfgs) != 2 {
		t.Fatalf("got %d configs", len(cfgs))
	}
	for i, c := range cfgs {
		if c.Listen != f.Listen || c.DockerSocket != f.DockerSocket || c.CloudflareAPIURL != f.CloudflareAPIURL || c.TelegramAPIURL != f.TelegramAPIURL || c.ApprovalTTL != f.ApprovalTTL || len(c.ComposeCommand) != 1 || c.ComposeCommand[0] != "/opt/compose" {
			t.Errorf("config %d missing shared settings: %+v", i, c)
		}
	}
	a, b := cfgs[0], cfgs[1]
	if a.Project != "shop" || a.Environment != "prod" || a.StateDir != "/s/prod" || a.KeyFile != "/k/prod" || !a.Protected || a.Network != "shop-prod" || len(a.Tokens) != 1 || a.Tokens[0].Scope != "admin" {
		t.Errorf("first %+v", a)
	}
	if a.ID() != (scope.ID{Project: "shop", Environment: "prod"}) {
		t.Errorf("first id %+v", a.ID())
	}
	if b.Project != "blog" || b.Environment != "dev" || b.Protected || b.Network != "" || b.ID() != (scope.ID{Project: "blog", Environment: "dev"}) {
		t.Errorf("second %+v id %+v", b, b.ID())
	}
}

func TestParseFileRefusesUnknownKeys(t *testing.T) {
	d := Digest("admin")
	hosted := `{"project":"shop","environment":"prod","state_dir":"/s","key_file":"/k","tokens":[{"digest":"` + d + `","scope":"admin"}]}`
	base := `{"listen":"127.0.0.1:9999","docker_socket":"/var/run/docker.sock","environments":[` + hosted + `]}`
	for _, body := range []string{
		`{"listen":"127.0.0.1:9999","docker_socket":"/var/run/docker.sock","environments":[` + hosted + `],"extra":1}`,
		`{"listen":"127.0.0.1:9999","docker_socket":"/var/run/docker.sock","environments":[{"project":"shop","environment":"prod","state_dir":"/s","key_file":"/k","tokens":[{"digest":"` + d + `","scope":"admin"}],"wat":1}]}`,
		base + `{}`,
	} {
		if _, err := ParseFile([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func testHosted(project, env, suffix string) Hosted {
	return Hosted{
		Project:     project,
		Environment: env,
		StateDir:    "/var/lib/berthy/" + suffix,
		KeyFile:     "/etc/berthy/" + suffix + ".key",
		Network:     suffix,
		Tokens:      []Token{{Digest: Digest(suffix), Scope: "admin"}},
	}
}

func testFile(envs ...Hosted) File {
	return File{
		Listen:       "127.0.0.1:9999",
		DockerSocket: "/var/run/docker.sock",
		Environments: envs,
	}
}
