package main

import (
	"bufio"
	"context"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/adapter"
)

func TestProxyOwner(t *testing.T) {
	// 真机抓的：系统代理被别的工具占着，HTTP 和 HTTPS 都指向 127.0.0.1:1082。
	occupied, err := os.ReadFile("testdata/scutil-proxy-occupied.txt")
	if err != nil {
		t.Fatal(err)
	}
	pac := "<dictionary> {\n  ProxyAutoConfigEnable : 1\n  ProxyAutoConfigURLString : http://127.0.0.1:9/proxy.pac\n}\n"
	none := "<dictionary> {\n  HTTPEnable : 0\n  HTTPSEnable : 0\n  ProxyAutoConfigEnable : 0\n  SOCKSEnable : 0\n}\n"
	cases := []struct {
		name, scutil, listen, want string
	}{
		{"被别的工具占用", string(occupied), "127.0.0.1:11080", "127.0.0.1:1082"},
		{"指向的就是自己", string(occupied), "127.0.0.1:1082", ""},
		{"用的是 PAC", pac, "127.0.0.1:11080", "http://127.0.0.1:9/proxy.pac"},
		{"没开系统代理", none, "127.0.0.1:11080", ""},
	}
	for _, c := range cases {
		if got := proxyOwner(parseScutil([]byte(c.scutil)), c.listen); got != c.want {
			t.Errorf("%s: proxyOwner = %q，想要 %q", c.name, got, c.want)
		}
	}
}

// loginForm 和真实服务器发来的表单同构：用户名、密码，再加一个预选了第一项的认证组下拉框。
func loginForm() *adapter.OpenConnectAuthChallenge {
	return &adapter.OpenConnectAuthChallenge{
		ID:      "1",
		Message: "Please enter your username and password",
		Form: &adapter.OpenConnectAuthForm{Fields: []adapter.OpenConnectAuthFormField{
			{SubmissionKey: "k-user", Name: "username", Label: "Username:", Kind: "text"},
			{SubmissionKey: "k-pass", Name: "password", Label: "Password:", Kind: "password"},
			{SubmissionKey: "k-group", Name: "group_list", Label: "GROUP:", Kind: "select", Value: "staff",
				Options: []adapter.OpenConnectAuthFormChoice{{Value: "staff", Label: "Staff"}, {Value: "student", Label: "Student"}}},
		}},
	}
}

func TestAskChallenge(t *testing.T) {
	cases := []struct {
		name, input string
		want        credentials
	}{
		{"选第二个组", "alice\ns3cret pass\n2\n", credentials{"alice", "s3cret pass", "student"}},
		{"直接回车用预选的组", "bob\npw\n\n", credentials{"bob", "pw", "staff"}},
		{"编号输错了重新问", "carol\npw\n9\n1\n", credentials{"carol", "pw", "staff"}},
	}
	for _, c := range cases {
		var creds credentials
		values, err := askChallenge(loginForm(), bufio.NewReader(strings.NewReader(c.input)), &creds, map[string]bool{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if creds != c.want {
			t.Errorf("%s: 认出的凭据 %+v，想要 %+v", c.name, creds, c.want)
		}
		want := map[string]string{"k-user": c.want.username, "k-pass": c.want.password, "k-group": c.want.group}
		if !maps.Equal(values, want) {
			t.Errorf("%s: 提交的表单 %v，想要 %v", c.name, values, want)
		}
	}
}

func TestAskChallengeRejectsBrowserLogin(t *testing.T) {
	ch := &adapter.OpenConnectAuthChallenge{ID: "1", Browser: &adapter.OpenConnectBrowserRequest{URL: "https://sso.example.edu/"}}
	if _, err := askChallenge(ch, bufio.NewReader(strings.NewReader("")), &credentials{}, map[string]bool{}); err == nil {
		t.Fatal("要求浏览器登录时应当报错，而不是空着提交")
	}
}

// TestAskChallengeReusesGroup 复现真实服务器的两轮表单：第一轮只问组，第二轮发来
// 组、用户名、密码俱全的完整表单。第一轮选过的组不该再问一遍。
func TestAskChallengeReusesGroup(t *testing.T) {
	full := loginForm()
	groupOnly := &adapter.OpenConnectAuthChallenge{ID: "0", Form: &adapter.OpenConnectAuthForm{
		Fields: []adapter.OpenConnectAuthFormField{full.Form.Fields[2]},
	}}
	full.Form.Fields = []adapter.OpenConnectAuthFormField{full.Form.Fields[2], full.Form.Fields[0], full.Form.Fields[1]}

	in := bufio.NewReader(strings.NewReader("2\nalice\npw\n"))
	var creds credentials
	seen := map[string]bool{}
	if _, err := askChallenge(groupOnly, in, &creds, seen); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	values, err := askChallenge(full, in, &creds, seen)
	if err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if want := (credentials{"alice", "pw", "student"}); creds != want {
		t.Errorf("认出的凭据 %+v，想要 %+v", creds, want)
	}
	if want := map[string]string{"k-group": "student", "k-user": "alice", "k-pass": "pw"}; !maps.Equal(values, want) {
		t.Errorf("第二轮提交的表单 %v，想要 %v", values, want)
	}
}

// rejectingServer 是一台一直拒绝登录的假服务器：每提交一次，就带着 "Login failed." 重发一份
// 表单，和密码错了时真实服务器的反应一样。
type rejectingServer struct {
	mu        sync.Mutex
	submitted int
	updated   chan struct{}
}

func newRejectingServer() *rejectingServer { return &rejectingServer{updated: make(chan struct{})} }

func (s *rejectingServer) OpenConnectStatus() adapter.OpenConnectStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := loginForm()
	ch.ID = strconv.Itoa(s.submitted)
	if s.submitted > 0 {
		ch.Error = "Login failed."
	}
	return adapter.OpenConnectStatus{State: adapter.OpenConnectStateAuthPending, AuthChallenge: ch}
}

func (s *rejectingServer) StatusUpdated() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updated
}

func (s *rejectingServer) CompleteAuthChallenge(string, adapter.OpenConnectAuthResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitted++
	close(s.updated)
	s.updated = make(chan struct{})
	return nil
}

func (s *rejectingServer) CancelAuthChallenge(string) error { return nil }

// TestLoginStopsAfterRepeatedRejections 钉住「setup 登录失败没有次数上限」：服务器每拒绝一次就
// 重发一份表单，setup 就接着问，T4 的自动脚本在 6 分钟里提交了 519 次。学校账号连续输错会被
// 锁（后台服务被拒时也是这个原因不再重试），连续被拒 3 次就该停下，说明原因。
func TestLoginStopsAfterRepeatedRejections(t *testing.T) {
	server := newRejectingServer()
	// 够提交 10 次的输入：第一轮还要选组（回车取预选的），之后沿用。
	input := "alice\npw\n\n" + strings.Repeat("alice\npw\n", 9)
	var err error
	out := captureStdout(t, func() {
		_, _, err = answerLogin(context.Background(), server, bufio.NewReader(strings.NewReader(input)))
	})
	if server.submitted != 3 {
		t.Errorf("服务器一直拒绝时只该试 3 次，实际提交了 %d 次", server.submitted)
	}
	if err == nil || !strings.Contains(err.Error(), "连续 3 次") {
		t.Errorf("连续被拒 3 次应当停下并说明原因，实际返回 %v", err)
	}
	// 每次被拒都说清楚还能再试几次。
	for _, want := range []string{"还能再试 2 次", "还能再试 1 次"} {
		if !strings.Contains(out, want) {
			t.Errorf("被拒之后应当提示「%s」，实际输出 %q", want, out)
		}
	}
}

// TestAskMode 确认模式一定会问，而且推荐项跟着系统代理的状况走。普通用户也能直接选 tun：
// 要管理员权限的那几步，setup 会切换到 sudo 接着做，不再让用户换成 sudo 从头重来。
func TestAskMode(t *testing.T) {
	if modeUnsupported(ModeSystem) != "" {
		t.Skip("这个系统没有 system 模式，见 TestAskModeWithoutSystem")
	}
	cases := []struct {
		name, owner, input, want string
		wantErr                  bool
	}{
		{"没人占用系统代理，回车选推荐的 system", "", "\n", ModeSystem, false},
		{"系统代理被占用，回车选推荐的 proxy", "127.0.0.1:1082", "\n", ModeProxy, false},
		{"手动选 proxy", "", "1\n", ModeProxy, false},
		{"不加 sudo 也能选 tun", "", "3\n", ModeTUN, false},
	}
	for _, c := range cases {
		got, err := askMode(bufio.NewReader(strings.NewReader(c.input)), "127.0.0.1:11080", c.owner)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: askMode = %q, %v；想要 %q（报错 %v）", c.name, got, err, c.want, c.wantErr)
		}
	}
}

// TestAskModeWithoutSystem：没有 system 模式的系统（比如 Linux）上，菜单里只有 proxy 和 tun，推荐 proxy。
func TestAskModeWithoutSystem(t *testing.T) {
	if modeUnsupported(ModeSystem) == "" {
		t.Skip("这个系统有 system 模式，见 TestAskMode")
	}
	cases := []struct {
		name, input, want string
		wantErr           bool
	}{
		{"回车选推荐的 proxy", "\n", ModeProxy, false},
		{"第 2 项是 tun，不加 sudo 也能选", "2\n", ModeTUN, false},
	}
	for _, c := range cases {
		got, err := askMode(bufio.NewReader(strings.NewReader(c.input)), "127.0.0.1:11080", "")
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: askMode = %q, %v；想要 %q（报错 %v）", c.name, got, err, c.want, c.wantErr)
		}
	}
}
