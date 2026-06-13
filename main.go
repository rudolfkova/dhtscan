// dht-scan is a tiny, self-contained probe that tells you whether a host's
// network is ready to talk to TON storage providers the same way
// mytonstorage-backend does when it calls RequestStorageInfo:
//
//	1. resolve the provider in DHT  (FindValue "storage-provider" + FindAddresses)
//	2. open an ADNL/RLDP session    (RegisterClient + GetStorageRates)
//
// It mirrors tonutils-storage-provider/pkg/transport.Client.connect exactly,
// so a green verdict here means the real notify path will work too.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"
)

type options struct {
	host        string
	limit       int
	uptime      float64
	pubkeysCSV  string
	configURL   string
	port        string
	concurrency int
	timeout     time.Duration
	doRLDP      bool
}

// stage names used in reports.
const (
	stageFindValue = "dht_findvalue"
	stageFindAddr  = "dht_findaddr"
	stageRegister  = "adnl_register"
	stageRLDP      = "rldp_query"
	stageOK        = "ok"
)

type provResult struct {
	pubkey       string
	findValueOK  bool
	findValueMs  int64
	findAddrOK   bool
	findAddrMs   int64
	resolvedAddr string
	rldpOK       bool
	rldpMs       int64
	failedStage  string
	errMsg       string
}

func main() {
	opt := parseFlags()

	fmt.Println(strings.Repeat("=", 72))
	fmt.Println(" dht-scan — TON DHT/ADNL readiness probe")
	fmt.Println(strings.Repeat("=", 72))
	printEnv(opt)

	// Stage 1: outbound TCP sanity + load TON global config.
	cfgCtx, cfgCancel := context.WithTimeout(context.Background(), 20*time.Second)
	cfg, err := liteclient.GetConfigFromUrl(cfgCtx, opt.configURL)
	cfgCancel()
	if err != nil {
		stageFail("load TON global config (outbound TCP)", err)
		fmt.Println()
		printVerdict("ENVIRONMENT BROKEN", []string{
			"Cannot even fetch the TON global config over HTTPS.",
			"This is basic outbound TCP/DNS, not DHT. Fix internet egress / DNS first.",
		}, []string{"scripts/check-egress-ip.sh"})
		os.Exit(3)
	}
	stageOk(fmt.Sprintf("loaded TON global config from %s", opt.configURL))

	// Stage 2: bind UDP listener (the socket all DHT/ADNL traffic uses).
	udpAddr := "0.0.0.0:" + opt.port
	dl, err := adnl.DefaultListener(udpAddr)
	if err != nil {
		stageFail(fmt.Sprintf("bind UDP listener on %s", udpAddr), err)
		fmt.Println()
		printVerdict("ENVIRONMENT BROKEN", []string{
			fmt.Sprintf("Could not bind UDP %s.", udpAddr),
			"Port is busy or not permitted. Try a different --port or free it.",
		}, []string{"scripts/net-udp-check.sh"})
		os.Exit(3)
	}
	defer dl.Close()
	stageOk(fmt.Sprintf("bound UDP listener on %s", udpAddr))

	netMgr := adnl.NewMultiNetReader(dl)

	// Stage 3: bring up DHT client over a dedicated gateway (random key),
	// plus a provider gateway (random key) for the RLDP session. Same wiring
	// as mytonprovider agent's ProviderTransport.
	_, dhtKey, _ := ed25519.GenerateKey(nil)
	dhtGate := adnl.NewGatewayWithNetManager(dhtKey, netMgr)
	if err := dhtGate.StartClient(); err != nil {
		stageFail("start DHT ADNL gateway", err)
		os.Exit(3)
	}
	defer dhtGate.Close()

	dhtClient, err := dht.NewClientFromConfig(dhtGate, cfg)
	if err != nil {
		stageFail("create DHT client (bootstrap)", err)
		fmt.Println()
		printVerdict("DHT NOT READY", []string{
			"DHT client could not bootstrap from config nodes.",
			"Almost always means outbound UDP to the internet is blocked.",
		}, []string{"scripts/net-udp-check.sh", "scripts/open-adnl-firewall.sh"})
		os.Exit(2)
	}
	stageOk("DHT client bootstrapped")

	_, provKey, _ := ed25519.GenerateKey(nil)
	provGate := adnl.NewGatewayWithNetManager(provKey, netMgr)
	if err := provGate.StartClient(); err != nil {
		stageFail("start provider ADNL gateway", err)
		os.Exit(3)
	}
	defer provGate.Close()

	// Stage 4: get the provider pubkeys to probe.
	pubkeys, src, err := loadPubkeys(opt)
	if err != nil {
		stageFail("load provider pubkeys", err)
		fmt.Println()
		printVerdict("ENVIRONMENT BROKEN", []string{
			"Could not obtain a provider list to probe.",
			"Pass --pubkeys=hex,hex,... manually, or check --host reachability.",
		}, []string{"scripts/check-egress-ip.sh"})
		os.Exit(3)
	}
	if len(pubkeys) == 0 {
		stageFail("load provider pubkeys", fmt.Errorf("got 0 providers"))
		os.Exit(3)
	}
	stageOk(fmt.Sprintf("got %d provider pubkeys (%s)", len(pubkeys), src))

	// Stage 5: probe each provider, replicating transport.Client.connect.
	fmt.Println()
	fmt.Printf("Probing %d providers (concurrency=%d, timeout=%s, rldp=%v)...\n\n",
		len(pubkeys), opt.concurrency, opt.timeout, opt.doRLDP)

	results := probeAll(dhtClient, provGate, pubkeys, opt)

	printPerProvider(results)
	printSummaryAndVerdict(results, opt)
}

func parseFlags() options {
	var opt options
	flag.StringVar(&opt.host, "host", "https://mytonprovider.org", "coordinator host for /api/v1/providers/search")
	flag.IntVar(&opt.limit, "limit", 20, "number of providers to fetch/probe")
	flag.Float64Var(&opt.uptime, "uptime", 50, "uptime_gt_percent filter for provider search")
	flag.StringVar(&opt.pubkeysCSV, "pubkeys", "", "comma-separated hex provider pubkeys (overrides API)")
	flag.StringVar(&opt.configURL, "config", "https://ton-blockchain.github.io/global.config.json", "TON global config URL")
	flag.StringVar(&opt.port, "port", "16167", "local UDP port for ADNL/DHT (0 = random)")
	flag.IntVar(&opt.concurrency, "concurrency", 8, "parallel provider probes")
	flag.DurationVar(&opt.timeout, "timeout", 10*time.Second, "per-provider timeout")
	flag.BoolVar(&opt.doRLDP, "rldp", true, "also run RLDP GetStorageRates after DHT resolve")
	flag.Parse()
	if opt.concurrency < 1 {
		opt.concurrency = 1
	}
	return opt
}

func printEnv(opt options) {
	fmt.Printf("  os/arch     : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  time        : %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("  udp port    : %s\n", opt.port)
	fmt.Printf("  ton config  : %s\n", opt.configURL)
	fmt.Printf("  provider src: %s\n", opt.host)
	fmt.Println()
}

func loadPubkeys(opt options) ([]string, string, error) {
	if strings.TrimSpace(opt.pubkeysCSV) != "" {
		var out []string
		for _, p := range strings.Split(opt.pubkeysCSV, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out, "from --pubkeys flag", nil
	}
	return fetchProviders(opt)
}

type searchReq struct {
	Filters map[string]any `json:"filters"`
	Sort    map[string]any `json:"sort"`
	Exact   []string       `json:"exact"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
}

type searchResp struct {
	Providers []struct {
		PubKey string `json:"pubkey"`
	} `json:"providers"`
}

func fetchProviders(opt options) ([]string, string, error) {
	body, _ := json.Marshal(searchReq{
		Filters: map[string]any{"uptime_gt_percent": opt.uptime},
		Sort:    map[string]any{"column": "rating", "order": "desc"},
		Exact:   []string{},
		Limit:   opt.limit,
		Offset:  0,
	})

	url := strings.TrimRight(opt.host, "/") + "/api/v1/providers/search"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}

	var sr searchResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, "", err
	}

	var out []string
	for _, p := range sr.Providers {
		if strings.TrimSpace(p.PubKey) != "" {
			out = append(out, p.PubKey)
		}
		if opt.limit > 0 && len(out) >= opt.limit {
			break
		}
	}
	return out, "from " + url, nil
}

func probeAll(dhtClient *dht.Client, provGate *adnl.Gateway, pubkeys []string, opt options) []provResult {
	results := make([]provResult, len(pubkeys))
	sem := make(chan struct{}, opt.concurrency)
	var wg sync.WaitGroup

	for i, pk := range pubkeys {
		wg.Add(1)
		go func(i int, pk string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeOne(dhtClient, provGate, pk, opt)
		}(i, pk)
	}
	wg.Wait()
	return results
}

func probeOne(dhtClient *dht.Client, provGate *adnl.Gateway, pubkeyHex string, opt options) provResult {
	res := provResult{pubkey: pubkeyHex, failedStage: stageOK}

	pub, err := hex.DecodeString(strings.TrimSpace(pubkeyHex))
	if err != nil || len(pub) != 32 {
		res.failedStage = "decode_pubkey"
		res.errMsg = fmt.Sprintf("invalid pubkey hex (len=%d): %v", len(pub), err)
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()

	// --- DHT layer step 1: FindValue("storage-provider") ---
	channelKeyID, err := tl.Hash(keys.PublicKeyED25519{Key: pub})
	if err != nil {
		res.failedStage = stageFindValue
		res.errMsg = "hash pubkey: " + err.Error()
		return res
	}

	t0 := time.Now()
	val, _, err := dhtClient.FindValue(ctx, &dht.Key{
		ID:    channelKeyID,
		Name:  []byte("storage-provider"),
		Index: 0,
	})
	res.findValueMs = time.Since(t0).Milliseconds()
	if err != nil {
		res.failedStage = stageFindValue
		res.errMsg = err.Error()
		return res
	}
	var rec transport.ProviderDHTRecord
	if _, err = tl.Parse(&rec, val.Data, true); err != nil {
		res.failedStage = stageFindValue
		res.errMsg = "parse dht record: " + err.Error()
		return res
	}
	res.findValueOK = true

	// --- DHT layer step 2: FindAddresses(adnl) ---
	t1 := time.Now()
	list, key, err := dhtClient.FindAddresses(ctx, rec.ADNLAddr)
	res.findAddrMs = time.Since(t1).Milliseconds()
	if err != nil {
		res.failedStage = stageFindAddr
		res.errMsg = err.Error()
		return res
	}
	addr, err := firstDialAddress(list)
	if err != nil {
		res.failedStage = stageFindAddr
		res.errMsg = err.Error()
		return res
	}
	res.findAddrOK = true
	res.resolvedAddr = addr

	if !opt.doRLDP {
		return res
	}

	// --- ADNL/RLDP layer: open session and run GetStorageRates ---
	peer, err := provGate.RegisterClient(addr, key)
	if err != nil {
		res.failedStage = stageRegister
		res.errMsg = err.Error()
		return res
	}

	t2 := time.Now()
	var rates transport.StorageRatesResponse
	rl := rldp.NewClientV2(peer)
	err = rl.DoQuery(ctx, 8196, transport.StorageRatesRequest{Size: 1}, &rates)
	res.rldpMs = time.Since(t2).Milliseconds()
	if err != nil {
		res.failedStage = stageRLDP
		res.errMsg = err.Error()
		return res
	}
	res.rldpOK = true
	return res
}

func firstDialAddress(list *address.List) (string, error) {
	if list == nil || len(list.Addresses) == 0 {
		return "", fmt.Errorf("empty address list")
	}
	for _, a := range list.Addresses {
		if s, err := address.DialString(a); err == nil {
			return s, nil
		}
	}
	return "", fmt.Errorf("no dialable addresses")
}

func printPerProvider(results []provResult) {
	for _, r := range results {
		mark := func(ok bool) string {
			if ok {
				return "OK  "
			}
			return "FAIL"
		}
		line := fmt.Sprintf("  %s | findvalue %s %5dms | findaddr %s %5dms",
			short(r.pubkey), mark(r.findValueOK), r.findValueMs, mark(r.findAddrOK), r.findAddrMs)
		if r.resolvedAddr != "" {
			line += " " + r.resolvedAddr
		}
		line += fmt.Sprintf(" | rldp %s %5dms", mark(r.rldpOK), r.rldpMs)
		if r.failedStage != stageOK && r.errMsg != "" {
			line += "  <" + r.failedStage + ": " + truncate(r.errMsg, 90) + ">"
		}
		fmt.Println(line)
	}
}

func printSummaryAndVerdict(results []provResult, opt options) {
	n := len(results)
	var fv, fa, rl int
	errSig := map[string]int{}
	var fvLat, faLat, rlLat []int64

	for _, r := range results {
		if r.findValueOK {
			fv++
			fvLat = append(fvLat, r.findValueMs)
		}
		if r.findAddrOK {
			fa++
			faLat = append(faLat, r.findAddrMs)
		}
		if r.rldpOK {
			rl++
			rlLat = append(rlLat, r.rldpMs)
		}
		if r.failedStage != stageOK {
			errSig[r.failedStage+": "+classify(r.errMsg)]++
		}
	}

	fmt.Println()
	fmt.Println(strings.Repeat("-", 72))
	fmt.Println(" SUMMARY")
	fmt.Println(strings.Repeat("-", 72))
	fmt.Printf("  providers probed     : %d\n", n)
	fmt.Printf("  DHT FindValue  OK    : %d/%d   %s\n", fv, n, latStr(fvLat))
	fmt.Printf("  DHT FindAddr   OK    : %d/%d   %s\n", fa, n, latStr(faLat))
	if opt.doRLDP {
		fmt.Printf("  RLDP GetRates  OK    : %d/%d   %s\n", rl, n, latStr(rlLat))
	}
	if len(errSig) > 0 {
		fmt.Println("  dominant errors      :")
		for _, kv := range sortedByCount(errSig) {
			fmt.Printf("      %3dx  %s\n", kv.count, kv.key)
		}
	}

	// Verdict.
	switch {
	case fv == 0:
		printVerdict("DHT NOT READY  (outbound UDP/DHT likely blocked)", []string{
			"Not a single provider resolved in DHT — the exact failure seen on the broken staging VM.",
			"DHT lookups go out over UDP; if zero resolve, UDP egress (or NAT return path) is blocked.",
		}, []string{"scripts/net-udp-check.sh", "scripts/open-adnl-firewall.sh", "scripts/check-egress-ip.sh"})
		os.Exit(2)
	case opt.doRLDP && rl == 0 && fa > 0:
		printVerdict("DEGRADED  (DHT resolves, provider RLDP unreachable)", []string{
			"DHT works, but no RLDP session could be established with any provider.",
			"Likely UDP to provider ports is filtered, or all sampled providers are down.",
		}, []string{"scripts/net-udp-check.sh", "scripts/check-egress-ip.sh"})
		os.Exit(2)
	case fv*2 < n:
		printVerdict("DEGRADED  (partial DHT resolution)", []string{
			fmt.Sprintf("Only %d/%d providers resolved in DHT — flaky UDP, packet loss, or strict NAT.", fv, n),
		}, []string{"scripts/net-udp-check.sh"})
		os.Exit(1)
	default:
		hint := "DHT/ADNL egress works on this host."
		if opt.doRLDP {
			hint = "DHT resolve and RLDP both work — RequestStorageInfo will succeed from here."
		}
		printVerdict("DHT-READY", []string{hint}, nil)
		os.Exit(0)
	}
}

func printVerdict(verdict string, notes, scripts []string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf(" VERDICT: %s\n", verdict)
	fmt.Println(strings.Repeat("=", 72))
	for _, l := range notes {
		fmt.Println("  " + l)
	}
	if len(scripts) > 0 {
		fmt.Println()
		fmt.Println("  Recommended next steps (run from the dht-scan directory):")
		for _, s := range scripts {
			fmt.Printf("      sudo bash %s\n", s)
		}
	}
	fmt.Println()
}

// classify normalizes raw transport errors into short, groupable signatures.
func classify(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "find storage-provider in dht"):
		return "provider not found in dht (deadline)"
	case strings.Contains(m, "find address in dht"):
		return "adnl address not found in dht (deadline)"
	case strings.Contains(m, "deadline") || strings.Contains(m, "timeout"):
		return "timeout / deadline exceeded"
	case strings.Contains(m, "no dialable") || strings.Contains(m, "address list"):
		return "no usable address from dht"
	case strings.Contains(m, "refused"):
		return "connection refused"
	default:
		return truncate(msg, 60)
	}
}

type kv struct {
	key   string
	count int
}

func sortedByCount(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

func latStr(v []int64) string {
	if len(v) == 0 {
		return ""
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	min := v[0]
	max := v[len(v)-1]
	med := v[len(v)/2]
	return fmt.Sprintf("(min %dms / med %dms / max %dms)", min, med, max)
}

func short(pk string) string {
	if len(pk) <= 12 {
		return pk
	}
	return pk[:8] + ".." + pk[len(pk)-2:]
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func stageOk(msg string)            { fmt.Printf("  [ OK ] %s\n", msg) }
func stageFail(msg string, e error) { fmt.Printf("  [FAIL] %s: %v\n", msg, e) }
