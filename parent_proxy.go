package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	ss "github.com/shadowsocks/shadowsocks-go/shadowsocks"
)

// Interface that all types of parent proxies should support.
type ParentProxy interface {
	connect(*URL) (net.Conn, error)
	getServer() string // for use in updating server latency
	genConfig() string // for upgrading config
}

// Interface for different proxy selection strategy.
type ParentPool interface {
	add(ParentProxy)
	empty() bool
	// Select a proxy from the pool and connect. May try several proxies until
	// one that succees, return nil and error if all parent proxies fail.
	connect(*URL) (net.Conn, error)
}

// Init parentProxy to be backup pool. So config parsing have a pool to add
// parent proxies.
var parentProxy ParentPool = &backupParentPool{}

func initParentPool() {
	backPool, ok := parentProxy.(*backupParentPool)
	if !ok {
		panic("initial parent pool should be backup pool")
	}
	if debug {
		printParentProxy(backPool.parent)
	}
	if len(backPool.parent) == 0 {
		info.Println("no parent proxy server")
		return
	}
	if len(backPool.parent) == 1 && config.LoadBalance != loadBalanceBackup {
		debug.Println("only 1 parent, no need for load balance")
		config.LoadBalance = loadBalanceBackup
	}

	switch config.LoadBalance {
	case loadBalanceHash:
		debug.Println("hash parent pool", len(backPool.parent))
		parentProxy = &hashParentPool{*backPool}
	case loadBalanceLatency:
		debug.Println("latency parent pool", len(backPool.parent))
		go updateParentProxyLatency()
		parentProxy = newLatencyParentPool(backPool.parent)
	}
}

func printParentProxy(parent []ParentWithFail) {
	debug.Println("avaiable parent proxies:")
	for _, pp := range parent {
		switch pc := pp.ParentProxy.(type) {
		case *shadowsocksParent:
			debug.Println("\tshadowsocks: ", pc.server)
		case *httpParent:
			debug.Println("\thttp parent: ", pc.server)
		case *socksParent:
			debug.Println("\tsocks parent: ", pc.server)
		case *cowParent:
			debug.Println("\tcow parent: ", pc.server)
		}
	}
}

type ParentWithFail struct {
	ParentProxy
	fail int
}

// Backup load balance strategy:
// Select proxy in the order they appear in config.
type backupParentPool struct {
	parent []ParentWithFail
}

func (pp *backupParentPool) empty() bool {
	return len(pp.parent) == 0
}

func (pp *backupParentPool) add(parent ParentProxy) {
	pp.parent = append(pp.parent, ParentWithFail{parent, 0})
}

func (pp *backupParentPool) connect(url *URL) (srvconn net.Conn, err error) {
	return connectInOrder(url, pp.parent, 0)
}

// Hash load balance strategy:
// Each host will use a proxy based on a hash value.
type hashParentPool struct {
	backupParentPool
}

func (pp *hashParentPool) connect(url *URL) (srvconn net.Conn, err error) {
	start := int(crc32.ChecksumIEEE([]byte(url.Host)) % uint32(len(pp.parent)))
	debug.Printf("hash host %s try %d parent first", url.Host, start)
	return connectInOrder(url, pp.parent, start)
}

func (parent *ParentWithFail) connect(url *URL) (srvconn net.Conn, err error) {
	const maxFailCnt = 30
	srvconn, err = parent.ParentProxy.connect(url)
	if err != nil {
		if parent.fail < maxFailCnt && !networkBad() {
			parent.fail++
		}
		return
	}
	parent.fail = 0
	return
}

func connectInOrder(url *URL, pp []ParentWithFail, start int) (srvconn net.Conn, err error) {
	const baseFailCnt = 9
	var skipped []int
	nproxy := len(pp)

	if nproxy == 0 {
		return nil, errors.New("no parent proxy")
	}

	for i := 0; i < nproxy; i++ {
		proxyId := (start + i) % nproxy
		parent := &pp[proxyId]
		// skip failed server, but try it with some probability
		if parent.fail > 0 && rand.Intn(parent.fail+baseFailCnt) != 0 {
			skipped = append(skipped, proxyId)
			continue
		}
		if srvconn, err = parent.connect(url); err == nil {
			return
		}
	}
	// last resort, try skipped one, not likely to succeed
	for _, skippedId := range skipped {
		if srvconn, err = pp[skippedId].connect(url); err == nil {
			return
		}
	}
	return nil, err
}

type ParentWithLatency struct {
	ParentProxy
	latency time.Duration
}

type latencyParentPool struct {
	parent []ParentWithLatency
}

func newLatencyParentPool(parent []ParentWithFail) *latencyParentPool {
	lp := &latencyParentPool{}
	for _, p := range parent {
		lp.add(p.ParentProxy)
	}
	return lp
}

func (pp *latencyParentPool) empty() bool {
	return len(pp.parent) == 0
}

func (pp *latencyParentPool) add(parent ParentProxy) {
	pp.parent = append(pp.parent, ParentWithLatency{parent, 0})
}

// Sort interface.
func (pp *latencyParentPool) Len() int {
	return len(pp.parent)
}

func (pp *latencyParentPool) Swap(i, j int) {
	p := pp.parent
	p[i], p[j] = p[j], p[i]
}

func (pp *latencyParentPool) Less(i, j int) bool {
	p := pp.parent
	return p[i].latency < p[j].latency
}

const latencyMax = time.Hour

var latencyMutex sync.RWMutex

func (pp *latencyParentPool) connect(url *URL) (srvconn net.Conn, err error) {
	var lp []ParentWithLatency
	// Read slice first.
	latencyMutex.RLock()
	lp = pp.parent
	latencyMutex.RUnlock()

	var skipped []int
	nproxy := len(lp)
	if nproxy == 0 {
		return nil, errors.New("no parent proxy")
	}

	for i := 0; i < nproxy; i++ {
		parent := lp[i]
		if parent.latency >= latencyMax {
			skipped = append(skipped, i)
			continue
		}
		if srvconn, err = parent.connect(url); err == nil {
			debug.Println("lowest latency proxy", parent.getServer())
			return
		}
		parent.latency = latencyMax
	}
	// last resort, try skipped one, not likely to succeed
	for _, skippedId := range skipped {
		if srvconn, err = lp[skippedId].connect(url); err == nil {
			return
		}
	}
	return nil, err
}

func (parent *ParentWithLatency) updateLatency(wg *sync.WaitGroup) {
	defer wg.Done()
	proxy := parent.ParentProxy
	server := proxy.getServer()

	host, port, err := net.SplitHostPort(server)
	if err != nil {
		panic("split host port parent server error" + err.Error())
	}

	// Resolve host name first, so latency does not include resolve time.
	ip, err := net.LookupHost(host)
	if err != nil {
		parent.latency = latencyMax
		return
	}
	ipPort := net.JoinHostPort(ip[0], port)

	const N = 3
	var total time.Duration
	for i := 0; i < N; i++ {
		now := time.Now()
		cn, err := net.DialTimeout("tcp", ipPort, dialTimeout)
		if err != nil {
			debug.Println("latency update dial:", err)
			total += time.Minute // 1 minute as penalty
			continue
		}
		total += time.Now().Sub(now)
		cn.Close()

		time.Sleep(5 * time.Millisecond)
	}
	parent.latency = total / N
	debug.Println("latency", server, parent.latency)
}

func (pp *latencyParentPool) updateLatency() {
	// Create a copy, update latency for the copy.
	var cp latencyParentPool
	cp.parent = append(cp.parent, pp.parent...)

	// cp.parent is value instead of pointer, if we use `_, p := range cp.parent`,
	// the value in cp.parent will not be updated.
	var wg sync.WaitGroup
	wg.Add(len(cp.parent))
	for i, _ := range cp.parent {
		cp.parent[i].updateLatency(&wg)
	}
	wg.Wait()

	// Sort according to latency.
	sort.Stable(&cp)
	debug.Println("latency lowest proxy", cp.parent[0].getServer())

	// Update parent slice.
	latencyMutex.Lock()
	pp.parent = cp.parent
	latencyMutex.Unlock()
}

func updateParentProxyLatency() {
	lp, ok := parentProxy.(*latencyParentPool)
	if !ok {
		return
	}

	for {
		lp.updateLatency()
		time.Sleep(60 * time.Second)
	}
}

// http parent proxy

// socks5 parent proxy
// socksParent 實作 ParentProxy.connect，直接撥號至 socks5 代理並在其上發起到目標站點的 CONNECT
func (sp *socksParent) connect(url *URL) (net.Conn, error) {
    // 以繁體中文註解：此方法建立到 socks5 代理伺服器的 TCP 連線，然後執行 socks5 認證與 CONNECT 握手，最後返回與目標站點已建立的隧道連線。
    c, err := net.Dial("tcp", sp.server)
    if err != nil {
        return nil, err
    }
    var hasErr bool
    defer func() {
        // 若中途失敗，需關閉連線，避免資源洩漏
        if hasErr {
            _ = c.Close()
        }
    }()

    // 1) 方法選擇（greeting），支援無認證與使用者/密碼認證
    var n int
    if sp.user != "" {
        // 宣告支援 username/password 認證
        greet := []byte{0x5, 0x1, 0x2}
        if n, err = c.Write(greet); n != 3 || err != nil {
            hasErr = true
            return nil, err
        }
    } else {
        if n, err = c.Write(socksMsgVerMethodSelection); n != 3 || err != nil {
            hasErr = true
            return nil, err
        }
    }

    // 2) 讀取方法選擇回覆（必須完整讀滿 2 bytes）
    rep := make([]byte, 2)
    if _, err = io.ReadFull(c, rep); err != nil {
        hasErr = true
        return nil, err
    }
    if rep[0] != 5 { // 版本需為 5
        hasErr = true
        return nil, socksProtocolErr
    }
    if sp.user != "" {
        // 需要 username/password 認證
        if rep[1] != 2 { // server 未接受帳密認證
            hasErr = true
            return nil, errors.New("socks server does not support username/password auth")
        }
        ulen := len(sp.user)
        plen := len(sp.pass)
        if ulen > 255 || plen > 255 {
            hasErr = true
            return nil, errors.New("username or password too long for socks5 auth")
        }
        authReq := make([]byte, 3+ulen+plen)
        authReq[0] = 0x1
        authReq[1] = byte(ulen)
        copy(authReq[2:], sp.user)
        authReq[2+ulen] = byte(plen)
        copy(authReq[3+ulen:], sp.pass)
        if n, err = c.Write(authReq); n != len(authReq) || err != nil {
            hasErr = true
            return nil, err
        }
        // 認證回覆
        reply := make([]byte, 2)
        if _, err = io.ReadFull(c, reply); err != nil {
            hasErr = true
            return nil, err
        }
        if reply[0] != 1 || reply[1] != 0 { // 0 表示成功
            hasErr = true
            return nil, errors.New("socks5 username/password authentication failed")
        }
    } else {
        // 無帳密時，server 必須回覆 0（no auth）
        if rep[1] != 0 {
            hasErr = true
            return nil, socksProtocolErr
        }
    }

    // 3) 發送 CONNECT 請求（目的地為最終目標站點）
    host := url.Host
    port, err := strconv.Atoi(url.Port)
    if err != nil {
        hasErr = true
        return nil, err
    }
    hostLen := len(host)
    bufLen := 5 + hostLen + 2
    req := make([]byte, bufLen)
    req[0] = 5   // 版本
    req[1] = 1   // CONNECT
    req[2] = 0   // RSV
    req[3] = 3   // ATYP = domain name
    req[4] = byte(hostLen)
    copy(req[5:], host)
    binary.BigEndian.PutUint16(req[5+hostLen:5+hostLen+2], uint16(port))
    if n, err = c.Write(req); n != bufLen || err != nil {
        hasErr = true
        return nil, err
    }

    // 4) 讀取 CONNECT 回覆：先讀 2 bytes 檢查版本與狀態碼，再讀剩餘欄位避免殘留
    head := make([]byte, 2)
    if _, err = io.ReadFull(c, head); err != nil {
        hasErr = true
        return nil, errors.New("connection failed (by socks server " + sp.server + ") while reading reply header")
    }
    if head[0] != 5 {
        hasErr = true
        return nil, socksProtocolErr
    }
    if head[1] != 0 {
        hasErr = true
        code := int(head[1])
        if code >= 0 && code < len(socksError) {
            return nil, errors.New("socks5 connect failed: " + socksError[code])
        }
        return nil, socksProtocolErr
    }
    // 讀取 RSV, ATYP
    rest := make([]byte, 2)
    if _, err = io.ReadFull(c, rest); err != nil {
        hasErr = true
        return nil, err
    }
    atyp := rest[1]
    switch atyp {
    case 1: // IPv4
        buf := make([]byte, 4+2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    case 3: // DOMAINNAME
        // 先讀取長度再讀完整域名與 port
        lb := make([]byte, 1)
        if _, err = io.ReadFull(c, lb); err != nil {
            hasErr = true
            return nil, err
        }
        dlen := int(lb[0])
        buf := make([]byte, dlen+2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    case 4: // IPv6
        buf := make([]byte, 16+2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    default:
        hasErr = true
        return nil, socksProtocolErr
    }

    // 至此，c 已是到目標站點的隧道
    return socksConn{c, sp}, nil
}

func (sp *socksParent) getServer() string {
	return sp.server
}
// 產生 socks5 代理的升級配置字串（繁體中文註解）
// - 若包含帳號密碼，格式為 socks5://user:pass@host:port
// - 否則為 socks5://host:port
func (sp *socksParent) genConfig() string {
	if sp.user != "" {
		return fmt.Sprintf("proxy = socks5://%s:%s@%s", sp.user, sp.pass, sp.server)
	}
	return fmt.Sprintf("proxy = socks5://%s", sp.server)
}
// 以既有連線(baseConn)作為底層，對 socks5 端點代理進行握手，再 CONNECT 到最終目標
// 繁體中文說明：
// - 此方法用於三級代理情境：先連至 endpoint_proxy（已由 baseConn 建立），
//   然後於該連線上執行 socks5 認證與 CONNECT 請求，最終取得到目標站點的隧道。
// - 所有錯誤皆會被檢查，若中途失敗會關閉 baseConn 以避免資源洩漏。
func (sp *socksParent) connectWithBaseConn(url *URL, baseConn net.Conn) (net.Conn, error) {
    c := baseConn
    var hasErr bool
    defer func() {
        if hasErr {
            // 出錯時關閉連線，避免洩漏
            _ = c.Close()
        }
    }()

    var err error
    var n int

    // 1) 發送方法選擇（greeting），視是否需要帳密認證
    if sp.user != "" {
        greet := []byte{0x5, 0x1, 0x2} // VER=5, NMETHODS=1, METHODS=2(username/password)
        if n, err = c.Write(greet); n != 3 || err != nil {
            hasErr = true
            return nil, err
        }
    } else {
        if n, err = c.Write(socksMsgVerMethodSelection); n != 3 || err != nil {
            hasErr = true
            return nil, err
        }
    }

    // 2) 讀取方法選擇回覆（必須完整讀滿 2 bytes）
    rep := make([]byte, 2)
    if _, err = io.ReadFull(c, rep); err != nil {
        hasErr = true
        return nil, err
    }
    if rep[0] != 5 { // 版本必須為 5
        hasErr = true
        return nil, socksProtocolErr
    }

    // 2.1) 若需要帳密認證，發送帳密並檢查回覆
    if sp.user != "" {
        if rep[1] != 2 { // 伺服器未接受帳密認證
            hasErr = true
            return nil, errors.New("socks server does not support username/password auth")
        }
        ulen := len(sp.user)
        plen := len(sp.pass)
        if ulen > 255 || plen > 255 {
            hasErr = true
            return nil, errors.New("username or password too long for socks5 auth")
        }
        authReq := make([]byte, 3+ulen+plen)
        authReq[0] = 0x1
        authReq[1] = byte(ulen)
        copy(authReq[2:], sp.user)
        authReq[2+ulen] = byte(plen)
        copy(authReq[3+ulen:], sp.pass)
        if n, err = c.Write(authReq); n != len(authReq) || err != nil {
            hasErr = true
            return nil, err
        }
        reply := make([]byte, 2)
        if _, err = io.ReadFull(c, reply); err != nil {
            hasErr = true
            return nil, err
        }
        if reply[0] != 1 || reply[1] != 0 { // 0 代表認證成功
            hasErr = true
            return nil, errors.New("socks5 username/password authentication failed")
        }
    } else {
        // 無帳密時，server 應回覆 0 表示不需認證
        if rep[1] != 0 {
            hasErr = true
            return nil, socksProtocolErr
        }
    }

    // 3) 發送 CONNECT（ATYP=domain），目標為最終站點 url.Host:url.Port
    host := url.Host
    port, err := strconv.Atoi(url.Port)
    if err != nil {
        hasErr = true
        return nil, err
    }
    hostLen := len(host)
    bufLen := 5 + hostLen + 2
    req := make([]byte, bufLen)
    req[0] = 5   // VER
    req[1] = 1   // CMD=CONNECT
    req[2] = 0   // RSV
    req[3] = 3   // ATYP=DOMAINNAME
    req[4] = byte(hostLen)
    copy(req[5:], host)
    binary.BigEndian.PutUint16(req[5+hostLen:5+hostLen+2], uint16(port))
    if n, err = c.Write(req); n != bufLen || err != nil {
        hasErr = true
        return nil, err
    }

    // 4) 讀取 CONNECT 回覆：先讀滿前 2 bytes 檢查版本與狀態碼，
    //    再吃掉剩餘欄位（RSV, ATYP, BND.ADDR, BND.PORT）避免殘留在緩衝區。
    head := make([]byte, 2)
    if _, err = io.ReadFull(c, head); err != nil {
        hasErr = true
        return nil, errors.New("connection failed (by socks server " + sp.server + ") while reading reply header")
    }
    if head[0] != 5 {
        hasErr = true
        return nil, socksProtocolErr
    }
    if head[1] != 0 { // 非 0 表示連線失敗
        hasErr = true
        // 嘗試映射錯誤碼至可讀訊息
        code := int(head[1])
        if code >= 1 && code < len(socksError) {
            return nil, errors.New("socks connect failed: " + socksError[code])
        }
        return nil, socksProtocolErr
    }
    // 狀態 OK，繼續讀取 RSV 與 ATYP
    tail := make([]byte, 2)
    if _, err = io.ReadFull(c, tail); err != nil {
        hasErr = true
        return nil, err
    }
    atyp := tail[1]
    // 依 ATYP 讀取 BND.ADDR
    switch atyp {
    case 1: // IPv4
        buf := make([]byte, 4+2) // addr(4) + port(2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    case 3: // DOMAIN
        // 先讀取長度
        lbuf := make([]byte, 1)
        if _, err = io.ReadFull(c, lbuf); err != nil {
            hasErr = true
            return nil, err
        }
        l := int(lbuf[0])
        // 讀取 domain 與 port
        buf := make([]byte, l+2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    case 4: // IPv6
        buf := make([]byte, 16+2)
        if _, err = io.ReadFull(c, buf); err != nil {
            hasErr = true
            return nil, err
        }
    default:
        hasErr = true
        return nil, socksProtocolErr
    }

    // 成功建立隧道
    return socksConn{c, sp}, nil
}

// http parent proxy
type httpParent struct {
	server     string
	userPasswd string // for upgrade config
	authHeader []byte
}

type httpConn struct {
	net.Conn
	parent *httpParent
}

func (s httpConn) String() string {
	return "http parent proxy " + s.parent.server
}

func newHttpParent(server string) *httpParent {
	return &httpParent{server: server}
}

func (hp *httpParent) getServer() string {
	return hp.server
}

func (hp *httpParent) genConfig() string {
	if hp.userPasswd != "" {
		return fmt.Sprintf("proxy = http://%s@%s", hp.userPasswd, hp.server)
	} else {
		return fmt.Sprintf("proxy = http://%s", hp.server)
	}
}

func (hp *httpParent) initAuth(userPasswd string) {
	if userPasswd == "" {
		return
	}
	hp.userPasswd = userPasswd
	b64 := base64.StdEncoding.EncodeToString([]byte(userPasswd))
	hp.authHeader = []byte(headerProxyAuthorization + ": Basic " + b64 + CRLF)
}

func (hp *httpParent) connect(url *URL) (net.Conn, error) {
	c, err := net.Dial("tcp", hp.server)
	if err != nil {
		errl.Printf("can't connect to http parent %s for %s: %v\n",
			hp.server, url.HostPort, err)
		return nil, err
	}
	debug.Printf("connected to: %s via http parent: %s\n",
		url.HostPort, hp.server)
	return httpConn{c, hp}, nil
}

// shadowsocks parent proxy
type shadowsocksParent struct {
	server string
	method string // method and passwd are for upgrade config
	passwd string
	cipher *ss.Cipher
}

type shadowsocksConn struct {
	net.Conn
	parent *shadowsocksParent
}

func (s shadowsocksConn) String() string {
	return "shadowsocks proxy " + s.parent.server
}

// In order to use parent proxy in the order specified in the config file, we
// insert an uninitialized proxy into parent proxy list, and initialize it
// when all its config have been parsed.

func newShadowsocksParent(server string) *shadowsocksParent {
	return &shadowsocksParent{server: server}
}

func (sp *shadowsocksParent) getServer() string {
	return sp.server
}

func (sp *shadowsocksParent) genConfig() string {
	method := sp.method
	if method == "" {
		method = "table"
	}
	return fmt.Sprintf("proxy = ss://%s:%s@%s", method, sp.passwd, sp.server)
}

func (sp *shadowsocksParent) initCipher(method, passwd string) {
	sp.method = method
	sp.passwd = passwd
	cipher, err := ss.NewCipher(method, passwd)
	if err != nil {
		Fatal("create shadowsocks cipher:", err)
	}
	sp.cipher = cipher
}

func (sp *shadowsocksParent) connect(url *URL) (net.Conn, error) {
	c, err := ss.Dial(url.HostPort, sp.server, sp.cipher.Copy())
	if err != nil {
		errl.Printf("can't connect to shadowsocks parent %s for %s: %v\n",
			sp.server, url.HostPort, err)
		return nil, err
	}
	debug.Println("connected to:", url.HostPort, "via shadowsocks:", sp.server)
	return shadowsocksConn{c, sp}, nil
}

// cow parent proxy
type cowParent struct {
	server string
	method string
	passwd string
	cipher *ss.Cipher
}

type cowConn struct {
	net.Conn
	parent *cowParent
}

func (s cowConn) String() string {
	return "cow proxy " + s.parent.server
}

func newCowParent(srv, method, passwd string) *cowParent {
	cipher, err := ss.NewCipher(method, passwd)
	if err != nil {
		Fatal("create cow cipher:", err)
	}
	return &cowParent{srv, method, passwd, cipher}
}

func (cp *cowParent) getServer() string {
	return cp.server
}

func (cp *cowParent) genConfig() string {
	method := cp.method
	if method == "" {
		method = "table"
	}
	return fmt.Sprintf("proxy = cow://%s:%s@%s", method, cp.passwd, cp.server)
}

func (cp *cowParent) connect(url *URL) (net.Conn, error) {
	c, err := net.Dial("tcp", cp.server)
	if err != nil {
		errl.Printf("can't connect to cow parent %s for %s: %v\n",
			cp.server, url.HostPort, err)
		return nil, err
	}
	debug.Printf("connected to: %s via cow parent: %s\n",
		url.HostPort, cp.server)
	ssconn := ss.NewConn(c, cp.cipher.Copy())
	return cowConn{ssconn, cp}, nil
}

// For socks documentation, refer to rfc 1928 http://www.ietf.org/rfc/rfc1928.txt

var socksError = [...]string{
	1: "General SOCKS server failure",
	2: "Connection not allowed by ruleset",
	3: "Network unreachable",
	4: "Host unreachable",
	5: "Connection refused",
	6: "TTL expired",
	7: "Command not supported",
	8: "Address type not supported",
	9: "to X'FF' unassigned",
}

var socksProtocolErr = errors.New("socks protocol error")

var socksMsgVerMethodSelection = []byte{
	0x5, // version 5
	1,   // n method
	0,   // no authorization required
}

// socks5 parent proxy
// socksParent 結構，支援帳號密碼欄位
// 用於儲存 socks5 代理伺服器資訊與驗證資訊
// server 格式為 host:port，user/pass 為驗證資訊
// 若 user 為空則不進行驗證
 type socksParent struct {
	server string
	user   string // 帳號
	pass   string // 密碼
}
// socksConn 結構，包裝 socksParent 的連線
 type socksConn struct {
	net.Conn
	parent *socksParent
}
func (s socksConn) String() string {
	return "socks proxy " + s.parent.server
}
// newSocksParent 支援 socks5://user:pass@host:port 格式
func newSocksParent(server string) *socksParent {
	user := ""
	pass := ""
	addr := server
	if at := strings.LastIndex(server, "@"); at != -1 {
		auth := server[:at]
		addr = server[at+1:]
		if colon := strings.Index(auth, ":"); colon != -1 {
			user = auth[:colon]
			pass = auth[colon+1:]
		} else {
			user = auth
		}
	}
	return &socksParent{server: addr, user: user, pass: pass}
}
// httpParent 支援外部底層連線
func (hp *httpParent) connectWithBaseConn(url *URL, baseConn net.Conn) (net.Conn, error) {
    // 以繁體中文註解：
    // 在既有連線 baseConn（通常是指向 HTTP 父代理的 TCP 連線）上，
    // 向父代理發送 HTTP CONNECT 請求，目標為 url.HostPort，
    // 成功後返回已建立隧道的連線（httpConn 包裝）。
    c := baseConn
    // 構造 CONNECT 請求行與必要的標頭（Host、Proxy-Connection）。
    // 若父代理需要認證，則附加 Proxy-Authorization。
    req := []byte("CONNECT " + url.HostPort + " HTTP/1.1\r\n")
    req = append(req, []byte("Host: "+url.HostPort+"\r\n")...)
    req = append(req, []byte("Proxy-Connection: Keep-Alive\r\n")...)
    if hp.authHeader != nil {
        // 加入基本認證標頭
        req = append(req, hp.authHeader...)
    }
    req = append(req, []byte("\r\n")...)

    // 送出 CONNECT 請求
    if _, err := c.Write(req); err != nil {
        // 以繁體中文註解：寫入失敗則返回錯誤
        return nil, err
    }

    // 讀取回覆，直到讀到空行（\r\n\r\n），確認狀態碼為 200
    buf := make([]byte, 0, 2048)
    tmp := make([]byte, 512)
    // 設定最大讀取迴圈次數避免無限等待
    for i := 0; i < 16; i++ {
        n, err := c.Read(tmp)
        if n > 0 {
            buf = append(buf, tmp[:n]...)
            // 檢查是否讀到標頭結束
            if strings.Contains(string(buf), "\r\n\r\n") {
                break
            }
        }
        if err != nil {
            // 讀取過程出錯
            return nil, err
        }
    }
    // 確認狀態行為 200
    resp := string(buf)
    if !strings.HasPrefix(resp, "HTTP/1.1 200") && !strings.HasPrefix(resp, "HTTP/1.0 200") {
        // 未獲得 200，視為 CONNECT 失敗
        return nil, errors.New("HTTP CONNECT to parent failed for target " + url.HostPort)
    }
    // 隧道建立成功，返回 httpConn 包裝
    return httpConn{c, hp}, nil
}
// connectWithEndpointProxy 先經 parentProxy，再經 endpoint_proxy，最後到目標網站
func connectWithEndpointProxy(url *URL) (net.Conn, error) {
	if config.EndpointProxy != "" {
		endpoint := NewEndpointProxyParent(config.EndpointProxy)
		if endpoint == nil {
			return nil, errors.New("endpoint_proxy 配置解析失敗")
		}
		return endpoint.connect(url)
	}
	return parentProxy.connect(url)
}

// EndpointProxyParent 實現三級代理的 endpoint_proxy，支援 socks5/http 並可選賬號密碼
// 若未配置 endpoint_proxy，則不啟用三級代理
// 以繁體中文註解

type EndpointProxyParent struct {
	protocol string
	server   string
	user     string
	passwd   string
}

// NewEndpointProxyParent 根據配置初始化 EndpointProxyParent
func NewEndpointProxyParent(cfg string) *EndpointProxyParent {
	// 以繁體中文註解：解析 endpoint_proxy 配置字串
	var protocol, server, user, passwd string
	if strings.HasPrefix(cfg, "socks5://") {
		protocol = "socks5"
		cfg = strings.TrimPrefix(cfg, "socks5://")
	} else if strings.HasPrefix(cfg, "http://") {
		protocol = "http"
		cfg = strings.TrimPrefix(cfg, "http://")
	}
	if at := strings.LastIndex(cfg, "@"); at != -1 {
		userpass := cfg[:at]
		server = cfg[at+1:]
		if colon := strings.Index(userpass, ":"); colon != -1 {
			user = userpass[:colon]
			passwd = userpass[colon+1:]
		} else {
			user = userpass
		}
	} else {
		server = cfg
	}
	return &EndpointProxyParent{protocol, server, user, passwd}
}

// connect 連線到 endpoint_proxy，支援 socks5/http 並可選賬號密碼
func (epp *EndpointProxyParent) connect(url *URL) (net.Conn, error) {
    // 以繁體中文註解：根據協議建立連線，並捕獲異常
    // 這裡的關鍵在於：若系統已配置上游父代理（proxy），
    // 我們應先經由父代理連到 endpoint_proxy（形成第二跳），
    // 然後在該連線上再執行 socks5/http CONNECT 到最終站點（第三跳）。
    // 若未配置父代理，則直接撥號 endpoint_proxy。
    var baseConn net.Conn
    var err error

    // 決定如何取得到底層連線 baseConn（直連或經由父代理）
    if !parentProxy.empty() {
        // 透過上層父代理先建立到 endpoint_proxy 伺服器的連線（或隧道）
        epURL := &URL{}
        epURL.ParseHostPort(epp.server)
        debug.Printf("endpoint_proxy: connect to %s via parent proxy\n", epp.server)
        baseConn, err = parentProxy.connect(epURL)
        if err != nil {
            return nil, err
        }
        // 若父代理是 HTTP（或 cow），則需要在此連線上先發送 CONNECT 到 endpoint，以建立第二跳隧道。
        switch bc := baseConn.(type) {
        case httpConn:
            // 使用父代理的 httpParent，在 baseConn 上對 endpoint 送 CONNECT
            hp := bc.parent
            // 在父代理上建立到 endpoint 的隧道
            tunneledConn, err := hp.connectWithBaseConn(epURL, baseConn)
            if err != nil {
                _ = baseConn.Close()
                return nil, err
            }
            baseConn = tunneledConn
        case cowConn:
            // cow 父代理亦採用 HTTP CONNECT 機制，這裡直接手工發送最小 CONNECT 標頭
            c := baseConn
            req := []byte("CONNECT " + epURL.HostPort + " HTTP/1.1\r\n")
            req = append(req, []byte("Host: "+epURL.HostPort+"\r\n")...)
            req = append(req, []byte("Proxy-Connection: Keep-Alive\r\n\r\n")...)
            if _, err := c.Write(req); err != nil {
                _ = c.Close()
                return nil, err
            }
            // 讀取回覆直到 \r\n\r\n，確認 200
            buf := make([]byte, 0, 2048)
            tmp := make([]byte, 512)
            for i := 0; i < 16; i++ {
                n, err := c.Read(tmp)
                if n > 0 {
                    buf = append(buf, tmp[:n]...)
                    if strings.Contains(string(buf), "\r\n\r\n") {
                        break
                    }
                }
                if err != nil {
                    _ = c.Close()
                    return nil, err
                }
            }
            resp := string(buf)
            if !strings.HasPrefix(resp, "HTTP/1.1 200") && !strings.HasPrefix(resp, "HTTP/1.0 200") {
                _ = c.Close()
                return nil, errors.New("HTTP CONNECT to cow parent failed for endpoint " + epURL.HostPort)
            }
            // 保持 baseConn 指向已建立隧道的連線
            baseConn = c
        default:
            // 非 HTTP 類型之父代理（如 socks/shadowsocks）通常已直接返回到 endpoint 的隧道，無需額外處理。
        }
    } else {
        // 無上層父代理，直接撥號到 endpoint_proxy 伺服器
        debug.Printf("endpoint_proxy: direct dial to %s\n", epp.server)
        baseConn, err = net.Dial("tcp", epp.server)
        if err != nil {
            return nil, err
        }
    }

    // 在 baseConn 之上執行對應協議的握手，目標為最終網站 url
    switch epp.protocol {
    case "socks5":
        // 使用 socksParent，並將 baseConn 作為底層 socket
        sp := newSocksParent(epp.server)
        // 若配置中帶有帳號密碼，必須注入到 socksParent
        sp.user = epp.user
        sp.pass = epp.passwd
        conn, err := sp.connectWithBaseConn(url, baseConn)
        if err != nil {
            _ = baseConn.Close()
            return nil, err
        }
        return conn, nil
    case "http":
        // 使用 httpParent，將 baseConn 作為底層 socket
        hp := newHttpParent(epp.server)
        if epp.user != "" || epp.passwd != "" {
            hp.initAuth(epp.user + ":" + epp.passwd)
        }
        conn, err := hp.connectWithBaseConn(url, baseConn)
        if err != nil {
            _ = baseConn.Close()
            return nil, err
        }
        return conn, nil
    default:
        _ = baseConn.Close()
        return nil, errors.New("不支援的 endpoint_proxy 協議")
    }
}
