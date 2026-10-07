package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
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

type fakeService struct{ uninstallErr error }

func (fakeService) Stop() error        { return nil }
func (f fakeService) Uninstall() error { return f.uninstallErr }

// TestUninstallServiceAlreadyGone 钉住「服务先被卸掉了，sidedoor uninstall 就再也删不掉剩下的配置
// 和密码」：emberbound 上一次卸载卸到一半（单元文件删了，systemctl daemon-reload 报错），之后每次
// 重跑都在 systemctl disable 这一步报「Unit sidedoor.service does not exist」，配置和密码一直留着。
// 服务文件已经不在了，卸载报的错就不该挡住后面的清理；服务文件还在时，失败就是失败。
func TestUninstallServiceAlreadyGone(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone.service")
	present := filepath.Join(dir, "present.service")
	if err := os.WriteFile(present, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("exit status 1")

	if err := uninstallService(fakeService{failed}, missing); !errors.Is(err, errServiceGone) {
		t.Errorf("服务文件已经不在了，应当报 errServiceGone 好让卸载接着清理；实际 %v", err)
	}
	if err := uninstallService(fakeService{failed}, present); err == nil || errors.Is(err, errServiceGone) {
		t.Errorf("服务文件还在时卸载失败，应当原样报错、停下来；实际 %v", err)
	}
	if err := uninstallService(fakeService{}, missing); err != nil {
		t.Errorf("正常卸掉时不该报错，实际 %v", err)
	}
}
