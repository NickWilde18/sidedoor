package main

import (
	"bufio"
	"maps"
	"os"
	"strings"
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
		values, err := askChallenge(loginForm(), bufio.NewReader(strings.NewReader(c.input)), &creds)
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
	if _, err := askChallenge(ch, bufio.NewReader(strings.NewReader("")), &credentials{}); err == nil {
		t.Fatal("要求浏览器登录时应当报错，而不是空着提交")
	}
}
