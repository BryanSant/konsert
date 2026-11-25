package shellint

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		// Ordinary commands are kept.
		{"ls -la /tmp", "ls -la /tmp"},
		{"sudo systemctl stop nginx", "sudo systemctl stop nginx"},
		{"git commit -m 'fix: things' --author='A <a@b.c>'", "git commit -m 'fix: things' --author='A <a@b.c>'"},
		{"git checkout 3f786850e387550fdab836ed7e6dc881de23001b", "git checkout 3f786850e387550fdab836ed7e6dc881de23001b"},
		{"docker run -p 8080:80 nginx", "docker run -p 8080:80 nginx"},
		{"cd ~/src/syre && make build", "cd ~/src/syre && make build"},
		{"kubectl logs 0190d1f2-7c3a-7b4e-9f00-1a2b3c4d5e6f", "kubectl logs 0190d1f2-7c3a-7b4e-9f00-1a2b3c4d5e6f"},
		{"echo $GITHUB_TOKEN", "echo $GITHUB_TOKEN"},
		{"docker login --password-stdin -u me", "docker login --password-stdin -u me"},
		{"pg_dump --no-password db", "pg_dump --no-password db"},
		{"gh auth login --with-token < token.txt", "gh auth login --with-token < token.txt"},
		{"tail -n 20 build.log | grep -i error", "tail -n 20 build.log | grep -i error"},

		// Secret-named flags.
		{"mytool --password=hunter2 --user bob", "mytool --password=… --user bob"},
		{"mytool --password hunter2 run", "mytool --password … run"},
		{"mytool --api-key abc123 list", "mytool --api-key … list"},
		{"vault login -method=userpass password=hunter2", "vault login -method=userpass password=…"},
		{"New-Thing -Name x -Password 'P@ss w0rd'", "New-Thing -Name x -Password …"},
		{"Connect-Thing -AccessToken:abcd", "Connect-Thing -AccessToken:…"},
		{"openssl enc -aes256 -pass pass:hunter2 -in f", "openssl enc -aes256 -pass … -in f"},
		{"tool --token -v", "tool --token -v"},
		{"tool --client-secret=s3cr3t", "tool --client-secret=…"},

		// Program-specific flags.
		{"mysql -uroot -phunter2 shop", "mysql -uroot -p… shop"},
		{"mysql -u root -p shop", "mysql -u root -p shop"},
		{"mysqldump -pab=cd db", "mysqldump -p… db"},
		{"docker login -u me -p hunter2 ghcr.io", "docker login -u me -p … ghcr.io"},
		{"sshpass -p hunter2 ssh host", "sshpass -p … ssh host"},
		{"curl -u bob:hunter2 https://x.example", "curl -u bob:… https://x.example"},
		{`curl -H "Authorization: Bearer abc.def" https://api.example`, "curl -H 'Authorization: …' https://api.example"},
		{`curl --header="X-Api-Key: k1" https://api.example`, "curl '--header=X-Api-Key: …' https://api.example"},
		{`curl -H "Accept: application/json" https://api.example`, `curl -H "Accept: application/json" https://api.example`},
		{"ConvertTo-SecureString 'hunter2' -AsPlainText -Force", "ConvertTo-SecureString … -AsPlainText -Force"},

		// Assignments.
		{"GITHUB_TOKEN=abc ./deploy.sh", "GITHUB_TOKEN=… ./deploy.sh"},
		{"export AWS_SECRET_ACCESS_KEY=abcd1234", "export AWS_SECRET_ACCESS_KEY=…"},
		{"OPENAI_API_KEY='x y' python app.py", "OPENAI_API_KEY=… python app.py"},
		{"FOO=bar make", "FOO=bar make"},
		{"PWD=/x env", "PWD=/x env"},
		{"$env:GITHUB_TOKEN = 'abc'", "$env:GITHUB_TOKEN = …"},
		{"$password = 'abc'", "$password = …"},
		{"npm config set //registry.npmjs.org/:_authToken=npm_abc", "npm config set //registry.npmjs.org/:_authToken=…"},
		{"docker run -e DB_PASSWORD=pw -e MODE=dev img", "docker run -e DB_PASSWORD=… -e MODE=dev img"},

		// URLs.
		{"git clone https://bob:hunter2@github.com/x/y.git", "git clone https://bob:…@github.com/x/y.git"},
		{"psql postgres://app:s3cret@db:5432/app", "psql postgres://app:…@db:5432/app"},
		{"DATABASE_URL=postgres://app:s3cret@db/app ./run", "DATABASE_URL=postgres://app:…@db/app ./run"},
		{"curl 'https://x.example/cb?code=1&access_token=abc&x=2'", "curl 'https://x.example/cb?code=1&access_token=…&x=2'"},
		{"git clone https://ghp_abcdefghijklmnopqrstuvwxyz0123456789@github.com/x/y", "git clone https://…@github.com/x/y"},
		{"curl https://example.com/a?b=c", "curl https://example.com/a?b=c"},

		// Opaque tokens.
		{"gh secret set X --body ghp_abcdefghijklmnopqrstuvwxyz0123456789", "gh secret set X --body …"},
		{"echo sk-ant-api03-abcdefghijklmnopqrstuv", "echo …"},
		{"aws configure set aws_access_key_id AKIAIOSFODNN7EXAMPLE", "aws configure set aws_access_key_id …"},
		{"tool verify eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "tool verify …"},
		{"tool unlock 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "tool unlock …"},
		{"tool unlock Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5MGFiY2RlZg==", "tool unlock …"},
		{"go test ./internal/worker/... -run TestRunnerStartTask", "go test ./internal/worker/... -run TestRunnerStartTask"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRedactTruncatesLongCommands(t *testing.T) {
	got := Redact("echo " + strings.Repeat("é", 5000))
	if n := len([]rune(got)); n != maxRedactedLen {
		t.Fatalf("len = %d runes, want %d", n, maxRedactedLen)
	}
	if !strings.HasSuffix(got, Redacted) {
		t.Fatalf("truncated summary should end with %q", Redacted)
	}
}

func TestSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"GITHUB_TOKEN": true, "api-key": true, "AccessToken": true, "_authToken": true,
		"password": true, "DB_PASS": true, "client_secret": true, "OPENAI_API_KEY": true,
		"password-stdin": false, "token-file": false, "key-id": false, "author": false,
		"PWD": false, "keyring": false, "no-password": false, "monkeys": false, "user": false,
	} {
		if got := secretName(name); got != want {
			t.Errorf("secretName(%q) = %v, want %v", name, got, want)
		}
	}
}
