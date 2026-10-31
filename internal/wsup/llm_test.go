package wsup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fxEnvelope(text string) []byte {
	data, _ := json.Marshal(map[string]any{"final_output": text, "output": "not the final answer", "usage": map[string]int{"input_tokens": 42}, "future_metadata": true})
	return data
}

func TestFXParsing(t *testing.T) {
	if !strings.Contains(namingSystem, "summary in Japanese") {
		t.Fatal("naming prompt must explicitly request a Japanese summary")
	}
	n, err := parseNaming(fxEnvelope(`{"branch":"topic/test","summary":"日本語"}`))
	if err != nil || n != (Naming{"topic/test", "日本語"}) {
		t.Fatalf("naming: %+v, %v", n, err)
	}
	r, err := parseResolution(fxEnvelope(`{"intent":"fix","plan":"explain","patch":"diff\n+change\n"}`))
	if err != nil || r.Patch != "diff\n+change\n" {
		t.Fatalf("resolution: %+v, %v", r, err)
	}
	if _, err := parseResolution(fxEnvelope(`{"intent":"fix","plan":"no change","patch":""}`)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"", "```json\n{\"branch\":\"x\",\"summary\":\"y\"}\n```",
		`null`, `[]`, `{}`, `{"branch":"x"}`, `{"branch":"x","summary":null}`,
		`{"branch":"x","summary":3}`, `{"branch":"x","summary":{}}`,
		`{"Branch":"x","summary":"y"}`, `{"branch":"x","summary":"y","extra":"z"}`,
		`{"branch":"x","branch":"z","summary":"y"}`, `{"branch":"","summary":"y"}`,
		`{"branch":"x","summary":" "}`, `{"branch":"x","summary":"y"} {}`,
		`{"branch":"x","summary":"y"`, `{"branch":"x","summary":"y",}`,
	} {
		if got, err := parseNaming(fxEnvelope(text)); err == nil || got != (Naming{}) {
			t.Errorf("accepted %q: %+v, %v", text, got, err)
		}
	}
	if _, err := strictStrings("{\"branch\":\"\xff\",\"summary\":\"y\"}", "branch", "summary"); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	for _, text := range []string{
		`{"intent":"x","plan":"y"}`, `{"intent":"x","plan":"y","patch":null}`,
		`{"intent":"","plan":"y","patch":""}`, `{"intent":"x","plan":" ","patch":""}`,
		`{"intent":"x","plan":"y","patch":"","extra":"bad"}`,
	} {
		if got, err := parseResolution(fxEnvelope(text)); err == nil || got != (Resolution{}) {
			t.Errorf("accepted resolution %q", text)
		}
	}
	for _, data := range []string{
		`{}`, `null`, `[]`, `{"output":"{\"branch\":\"x\",\"summary\":\"y\"}"}`,
		`{"final_output":null}`, `{"final_output":42}`, `{"final_output":" "}`,
		`{"final_output":"x"} {}`, `not json`, "\xff",
	} {
		if _, err := finalOutput([]byte(data)); err == nil {
			t.Errorf("accepted envelope %q", data)
		}
	}
}

func TestFXProfile(t *testing.T) {
	path := `/tmp/a"b\c) (allow process-exec) 日本語`
	q, err := sandboxQuote(path)
	if err != nil || q != `"/tmp/a\"b\\c) (allow process-exec) 日本語"` {
		t.Fatalf("quote: %s, %v", q, err)
	}
	for _, path := range []string{"relative", "/tmp/a\n", "/tmp/a\x00", "/tmp/a\t", "/tmp/\xff"} {
		if _, err := sandboxQuote(path); err == nil {
			t.Errorf("accepted path %q", path)
		}
	}
	profile, err := fxProfile("/opt/bin/fx", "/private/tmp/empty", []string{path, "/repo", "/git-common"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"(deny file-write*)", `(allow file-write* (subpath "/private/tmp/empty"))`,
		"(deny file-read*)", `(deny file-read* (subpath "/repo"))`,
		`(deny file-read* (subpath "/git-common"))`, "(deny process-fork)",
		`(deny process-exec)`, `(allow process-exec (literal "/opt/bin/fx"))`,
		"(deny file-read* (subpath " + q + "))",
	} {
		if !strings.Contains(profile, fragment) {
			t.Errorf("missing %s", fragment)
		}
	}
	if _, err := fxProfile("/fx", "/tmp", []string{"/bad\n"}); err == nil {
		t.Fatal("accepted profile injection")
	}
}

func TestFXEnvironmentAndPaths(t *testing.T) {
	t.Setenv("FX_CONFIG", "unsafe")
	t.Setenv("DYLD_INSERT_LIBRARIES", "unsafe")
	t.Setenv("HTTPS_PROXY", "unsafe")
	t.Setenv("OPENAI_API_KEY", "test-only-key")
	env := strings.Join(fxEnv("/private/tmp/isolated"), "\n")
	if strings.Contains(env, "unsafe") || !strings.Contains(env, "HOME=/private/tmp/isolated") || !strings.Contains(env, "OPENAI_API_KEY=test-only-key") {
		t.Fatalf("unexpected environment: %s", env)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalFXPath(link)
	want, _ := filepath.EvalSymlinks(dir)
	if err != nil || got != want {
		t.Fatalf("canonical path: %q, %v", got, err)
	}
	for _, path := range []string{"", "a\x00b", filepath.Join(dir, "missing")} {
		if _, err := canonicalFXPath(path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	wrapper := filepath.Join(dir, "fx")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := nativeFX(wrapper); err == nil {
		t.Fatal("accepted script")
	}
	if runtime.GOOS == "darwin" {
		t.Setenv("PATH", dir)
		if _, err := (FX{}).Name(context.Background(), "x", "y"); err == nil || !strings.Contains(err.Error(), "Mach-O") {
			t.Fatalf("script did not fail closed: %v", err)
		}
	}
}

func TestFXFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (FX{}).Name(ctx, "x", "y"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := (FX{}).Resolve(context.Background(), "not JSON"); err == nil {
		t.Fatal("accepted invalid context")
	}
	if runtime.GOOS != "darwin" {
		if _, err := (FX{}).Name(context.Background(), "x", "y"); err == nil || !strings.Contains(err.Error(), "refusing unsandboxed") {
			t.Fatalf("unsupported OS did not fail closed: %v", err)
		}
	}
	var b fxBuffer
	if _, ok := any(&b).(io.ReaderFrom); ok {
		t.Fatal("ReadFrom can bypass the output cap")
	}
	if _, err := b.Write(make([]byte, 8<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("unbounded output")
	}
}

// This fixture is a native executable, not a shell wrapper. It never calls a
// model. It actively probes the same enforced sandbox used by FX.ask.
const fxFixture = `package main
import("encoding/json"; "fmt"; "io"; "os"; "os/exec"; "path/filepath"; "strings"; "time")
func fail(s string) { fmt.Fprintln(os.Stderr,s); os.Exit(12) }
func main() {
 if len(os.Args)!=7 || strings.Join(os.Args[1:6]," ")!="ask --no-save --no-color --json --system" { fail("arguments") }
 data,_:=io.ReadAll(os.Stdin)
 var in map[string]string
 if json.Unmarshal(data,&in)!=nil { fail("stdin") }
 cwd,_:=os.Getwd()
 if cwd!=os.Getenv("HOME") || cwd!=os.Getenv("XDG_CONFIG_HOME") { fail("home") }
 if _,err:=os.Stat(filepath.Join(cwd,".fx")); err==nil { fail("inherited config") }
 if os.Getenv("FX_CONFIG")!="" || os.Getenv("WSUP_UNSAFE")!="" { fail("inherited env") }
 if in["mode"]=="sleep" { time.Sleep(30*time.Second) }
 if in["mode"]=="bad" { fmt.Print("not json"); return }
 if in["mode"]=="flood" { fmt.Print(strings.Repeat("x",9<<20)); return }
 if in["mode"]=="exit" { fail("secret diagnostics") }
 if in["mode"]=="audit" {
  for _,key:=range []string{"root","common","alias","home"} {
   if _,err:=os.ReadFile(in[key]); err==nil { fail("read "+key) }
  }
  if err:=os.WriteFile(in["outside"],[]byte("bad"),0600); err==nil { fail("outside write") }
  if err:=os.WriteFile(filepath.Join(cwd,"allowed"),[]byte("ok"),0600); err!=nil { fail("inside write: "+err.Error()) }
  for _,bin:=range []string{"/bin/sh","/usr/bin/git",os.Args[0]} {
   cmd:=exec.Command(bin,"--version")
   if err:=cmd.Start(); err==nil { cmd.Process.Kill(); cmd.Wait(); fail("child execution") }
  }
 }
 text:=""
 if _,ok:=in["context"]; ok {
  if in["intent"]!="name intent" || in["context"]!="context\n--system malicious" { fail("name context") }
  result,_:=json.Marshal(map[string]string{"branch":"topic/test","summary":cwd}); text=string(result)
 } else { result,_:=json.Marshal(map[string]string{"intent":"fix","plan":"fixture","patch":cwd}); text=string(result) }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"final_output":text,"output":"wrong","usage":nil})
}
`

func TestFXSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native sandbox enforcement test")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(fxFixture), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "fx")
	build := exec.Command("go", "build", "-o", binary, source)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FX_CONFIG", "unsafe")
	t.Setenv("WSUP_UNSAFE", "unsafe")
	root, common := t.TempDir(), t.TempDir()
	rootFile, commonFile := filepath.Join(root, "secret"), filepath.Join(common, "secret")
	for _, p := range []string{rootFile, commonFile} {
		if err := os.WriteFile(p, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	home := filepath.Join(dir, "user-config")
	if err := os.WriteFile(home, []byte("MCP config must not be inherited"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := FX{DenyPaths: []string{root, common}}
	probeDir, err := os.MkdirTemp("/private/tmp", "wsup-fx-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(probeDir)
	realBinary, _ := canonicalFXPath(binary)
	profile, err := fxProfile(realBinary, probeDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("/usr/bin/sandbox-exec", "-p", profile, realBinary, "ask", "--no-save", "--no-color", "--json", "--system", resolutionSystem)
	probe.Dir, probe.Env = probeDir, fxEnv(probeDir)
	probe.Stdin = strings.NewReader(`{}`)
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("native fixture startup: %v: %s", err, out)
	}
	n, err := engine.Name(context.Background(), "name intent", "context\n--system malicious")
	if err != nil || n.Branch != "topic/test" {
		t.Fatalf("sandbox Name: %+v, %v", n, err)
	}
	if _, err := os.Stat(n.Summary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable Name directory not removed: %v", err)
	}
	prompt, _ := json.Marshal(map[string]string{
		"mode": "audit", "root": rootFile, "common": commonFile,
		"alias": filepath.Join(alias, "secret"), "outside": outside, "home": home,
	})
	r, err := engine.Resolve(context.Background(), string(prompt))
	if err != nil || r.Plan != "fixture" {
		t.Fatalf("sandbox enforcement: %+v, %v", r, err)
	}
	if _, err := os.Stat(r.Patch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable Resolve directory not removed: %v", err)
	}
	if _, err := (FX{DenyPaths: []string{"/private"}}).Resolve(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("allowed temporary directory under denied path: %v", err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside write: %v", err)
	}
	for _, mode := range []string{"bad", "exit", "flood"} {
		if _, err := engine.Resolve(context.Background(), `{"mode":"`+mode+`"}`); err == nil || strings.Contains(err.Error(), "secret diagnostics") {
			t.Fatalf("%s failure not handled safely: %v", mode, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := engine.Resolve(ctx, `{"mode":"sleep"}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running cancellation: %v", err)
	}
	if _, err := (FX{DenyPaths: []string{filepath.Join(root, "missing")}}).Resolve(context.Background(), `{}`); err == nil {
		t.Fatal("missing deny path did not fail closed")
	}
}

// Opt in with WSUP_FX_SMOKE=1 go test -run TestFXInstalledSmoke -v ./internal/wsup.
// This runs ONLY --help and --version, never ask. The production profile and
// isolated environment are used, with credentials removed and network denied
// as an extra smoke-test precaution. No user configuration/auth files are read.
// Success proves CLI startup, not provider authentication or model requests.
func TestFXInstalledSmoke(t *testing.T) {
	if os.Getenv("WSUP_FX_SMOKE") != "1" {
		t.Skip("set WSUP_FX_SMOKE=1 to smoke-test the installed native fx without a model call")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("installed fx sandbox smoke test requires macOS")
	}
	binary, err := exec.LookPath("fx")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = canonicalFXPath(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeFX(binary); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err = canonicalFXPath(cwd)
	if err != nil {
		t.Fatal(err)
	}
	root, err := canonicalFXPath(filepath.Join(cwd, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AI_GATEWAY_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Logf("native executable: %s; denied repository: %s", binary, root)
	for _, flag := range []string{"--help", "--version"} {
		t.Run(flag, func(t *testing.T) {
			dir, err := os.MkdirTemp("/private/tmp", "wsup-fx-smoke-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Errorf("remove smoke directory: %v", err)
				}
			})
			profile, err := fxProfile(binary, dir, []string{cwd, root})
			if err != nil {
				t.Fatal(err)
			}
			profile += "\n(deny network*)\n"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, binary, flag)
			cmd.Dir, cmd.Env = dir, fxEnv(dir)
			cmd.WaitDelay = 2 * time.Second
			var stdout, stderr fxBuffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("sandboxed fx %s: %v; stdout=%q stderr=%q", flag, err, stdout.Bytes(), stderr.Bytes())
			}
			text := strings.TrimSpace(string(stdout.Bytes()))
			if text == "" || (flag == "--help" && !strings.Contains(text, "Usage:")) {
				t.Fatalf("unexpected fx %s output: %q", flag, text)
			}
			t.Logf("sandboxed fx %s: exit=%d; stdout first line=%q; stderr=%q; credentials absent; network denied", flag, cmd.ProcessState.ExitCode(), strings.SplitN(text, "\n", 2)[0], stderr.Bytes())
		})
	}
}
