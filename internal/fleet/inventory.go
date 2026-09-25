// inventory.go — host.list：巡视台唯一需要人维护的东西。
//
// 一台一行：
//
//	名字  地址[:端口]  键=值 …
//
// 键：dc（机房）、rack（机柜）、product（产品线）、group（主机组）、tags（逗号分隔，可多个）。
// # 后面是注释。地址可以是 10.1.0.5:8888，也可以是完整 URL（带反代、https、基本认证时用）。
//
// 纯文本、不用 YAML：零依赖，人手改不容易出错，也方便从 CMDB/Excel 导出。
package fleet

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Host 是清单里的一台。
type Host struct {
	Name    string   `json:"name"`
	URL     string   `json:"-"`    // 拉取用，可能带基本认证
	Addr    string   `json:"addr"` // 给页面看的，去掉了认证信息
	DC      string   `json:"dc,omitempty"`
	Rack    string   `json:"rack,omitempty"`
	Product string   `json:"product,omitempty"`
	Group   string   `json:"group,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,62}$`)

// defaultPort 是 nodedata serve 的默认端口。
const defaultPort = "8888"

// ParseInventory 读清单。坏行不进结果，每处问题写一条带行号的说明。
func ParseInventory(r io.Reader) (hosts []Host, errs []string) {
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Sprintf("第 %d 行：", ln)+fmt.Sprintf(format, a...))
		}
		if len(f) < 2 {
			bad("缺地址（格式：名字 地址:端口 键=值 …）")
			continue
		}
		if !hostName.MatchString(f[0]) {
			bad("主机名 %q 只能用字母、数字、点、横线、下划线", f[0])
			continue
		}
		if prev, dup := seen[f[0]]; dup {
			bad("主机名 %s 跟第 %d 行重复", f[0], prev)
			continue
		}
		u, show, err := normalizeAddr(f[1])
		if err != nil {
			bad("地址 %q 不对：%v", f[1], err)
			continue
		}
		h := Host{Name: f[0], URL: u, Addr: show}
		ok := true
		for _, kv := range f[2:] {
			k, v, found := strings.Cut(kv, "=")
			if !found || k == "" || v == "" {
				bad("%q 不是 键=值", kv)
				ok = false
				break
			}
			switch k {
			case "dc":
				h.DC = v
			case "rack":
				h.Rack = v
			case "product":
				h.Product = v
			case "group":
				h.Group = v
			case "tags":
				for _, t := range strings.Split(v, ",") {
					if t = strings.TrimSpace(t); t != "" {
						h.Tags = append(h.Tags, t)
					}
				}
			default:
				bad("不认识的键 %q（只有 dc rack product group tags）", k)
				ok = false
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		seen[h.Name] = ln
		hosts = append(hosts, h)
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, "读文件出错："+err.Error())
	}
	return hosts, errs
}

// normalizeAddr 把 10.1.0.5 / 10.1.0.5:8888 / http://… 统一成拉取用的 URL 根，
// 同时给出一份去掉用户名密码的显示用地址——密码不能跟着状态发到 iPad 上。
func normalizeAddr(s string) (fetch, show string, err error) {
	bare := !strings.Contains(s, "://")
	if bare {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("只支持 http/https")
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("没有主机部分")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("不要带 ? 或 #")
	}
	// 只写了 IP/主机名就补 nodedata 的默认端口；写成完整 URL 的（反代、https）照原样用。
	if bare && u.Port() == "" {
		u.Host += ":" + defaultPort
	}
	u.Path = strings.TrimRight(u.Path, "/")
	fetch = u.String()
	u.User = nil
	show = strings.TrimPrefix(u.String(), "http://")
	return fetch, show, nil
}

// Inventory 是当前在用的清单及其来历。
type Inventory struct {
	Path     string    `json:"path"`
	Hosts    []Host    `json:"-"`
	LoadedAt time.Time `json:"-"`
	// Errors 是最近一次读文件发现的问题。Stale=true 表示因为这些问题没有采用新文件，
	// 还在用 LoadedAt 那一版——一个笔误不能让一台机器从大屏上悄悄消失。
	Errors []string `json:"errors,omitempty"`
	Stale  bool     `json:"stale,omitempty"`
}

// invStore 管清单的热加载：文件变了就重读，读坏了就留着上一版。
type invStore struct {
	mu      sync.Mutex
	path    string
	cur     Inventory
	modTime time.Time
	size    int64
	loaded  bool
}

func newInvStore(path string) *invStore { return &invStore{path: path, cur: Inventory{Path: path}} }

// Get 返回当前清单（副本）。
func (s *invStore) Get() Inventory {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cur
	c.Hosts = append([]Host(nil), s.cur.Hosts...)
	c.Errors = append([]string(nil), s.cur.Errors...)
	return c
}

// Reload 看文件有没有变，变了就重读。返回清单是否换了新版本。
func (s *invStore) Reload(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := os.Stat(s.path)
	if err != nil {
		s.cur.Errors = []string{"读不到清单文件：" + err.Error()}
		s.cur.Stale = s.loaded
		s.modTime, s.size = time.Time{}, -1 // 文件回来后一定重读
		return false
	}
	if s.loaded && st.ModTime().Equal(s.modTime) && st.Size() == s.size {
		return false
	}
	f, err := os.Open(s.path)
	if err != nil {
		s.cur.Errors = []string{"打不开清单文件：" + err.Error()}
		s.cur.Stale = s.loaded
		s.modTime, s.size = time.Time{}, -1
		return false
	}
	hosts, errs := ParseInventory(f)
	f.Close()
	s.modTime, s.size = st.ModTime(), st.Size()

	// 已经有一版能用的：新文件有任何问题就整份不采用。
	// 首次启动没有旧版可退，能读的先用上，问题照样挂出来。
	if s.loaded && len(errs) > 0 {
		s.cur.Errors = errs
		s.cur.Stale = true
		return false
	}
	if len(hosts) == 0 && len(errs) == 0 {
		errs = []string{"清单里一台主机都没有"}
	}
	s.cur = Inventory{Path: s.path, Hosts: hosts, LoadedAt: now, Errors: errs}
	s.loaded = true
	return true
}
