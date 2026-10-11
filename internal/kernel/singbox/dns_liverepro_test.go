package singbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// TestReproLiveDNSFailure 完整复放线上内核启动：trojan inbound + TLS 证书 + 用户 +
// block/dns 线路——racknerd 上 dns/udp 报 "detour to an empty direct outbound"
func TestReproLiveDNSFailure(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := genSelfCert(t)
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	os.WriteFile(certPath, certPEM, 0600)
	os.WriteFile(keyPath, keyPEM, 0600)

	kcfg := config.KernelConfig{ConfigDir: dir}
	nc := &model.NodeSpec{
		Protocol: "trojan", ServerPort: 28443, // 沙箱空闲端口；dns 段与线上一致
		Network: "tcp",
		Routes: []model.RouteRule{
			{ID: 2, Match: []string{"*.doubleclick.net", "*.umeng.com"}, Action: "block"},
			{ID: 3, Match: []string{"*"}, Action: "dns", ActionValue: "1.1.1.1"},
		},
	}
	users := []model.UserSpec{{UUID: "11111111-2222-3333-4444-555555555555", SpeedLimit: 0}}

	cfg := buildConfig(kcfg, nc, users, kernel.TLSCert{CertPEM: certPEM, KeyPEM: keyPEM})
	data, _ := json.Marshal(cfg)
	t.Logf("dns段: %s", mustJSON(cfg["dns"]))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = include.Context(ctx)
	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	inst, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		t.Fatalf("box.New: %v", err)
	}
	defer inst.Close()
	if err := inst.Start(); err != nil {
		t.Fatalf("START FAILED（复现成功）: %v", err)
	}
	t.Log("START OK —— 线上错误未复现")
}

func genSelfCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "la01.aibolt.tech"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"la01.aibolt.tech"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
