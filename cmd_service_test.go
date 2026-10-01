package main

import (
	"net/url"
	"testing"
)

// TestDashboardURL 钉住「面板打不开」：面板自带的默认后台是 127.0.0.1:9090，网址里不带
// hostname、port、secret，它就去连 9090，而 sidedoor 的状态接口不在那。参数名取自面板
// 自己的代码（assets/index-*.js 里的 cp=["hostname","port","secret"]）。
func TestDashboardURL(t *testing.T) {
	cfg := &Config{API: "127.0.0.1:11081", APISecret: "S3CRET"}
	u, err := url.Parse(dashboardURL(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "127.0.0.1:11081" || u.Path != "/ui/" {
		t.Errorf("面板要从状态接口上打开，实际 %s", u)
	}
	want := map[string]string{"hostname": "127.0.0.1", "port": "11081", "secret": "S3CRET"}
	for k, v := range want {
		if got := u.Query().Get(k); got != v {
			t.Errorf("网址参数 %s = %q，想要 %q（完整网址 %s）", k, got, v, u)
		}
	}
}
