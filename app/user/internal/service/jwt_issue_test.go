package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"testing"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/transport"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// genTestRSAKeys 生成测试用 RSA 密钥对（PEM）。
func genTestRSAKeys(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privPEM, pubPEM
}

// openTestUserDB 内存 sqlite + 公共域组织 + 用户。
func openTestUserDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:jwt_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Org{}, &model.OrgMember{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pub := model.Org{Slug: model.PublicOrgSlug, Name: "公共域"}
	if err := db.Create(&pub).Error; err != nil {
		t.Fatalf("create public org: %v", err)
	}
	return db
}

// mockTransporter 构造带 Authorization header 的 kratos server context。
type mockHeader struct{ h http.Header }

func (m *mockHeader) Get(key string) string        { return m.h.Get(key) }
func (m *mockHeader) Set(key, value string)        { m.h.Set(key, value) }
func (m *mockHeader) Add(key, value string)        { m.h.Add(key, value) }
func (m *mockHeader) Keys() []string               { return []string{} }
func (m *mockHeader) Values(key string) []string   { return m.h.Values(key) }

type mockTransporter struct{ header *mockHeader }

func (m *mockTransporter) Kind() transport.Kind  { return transport.KindHTTP }
func (m *mockTransporter) Endpoint() string      { return "http://127.0.0.1:8000" }
func (m *mockTransporter) Operation() string     { return "/api.user.v1.Auth/Login" }
func (m *mockTransporter) RequestHeader() transport.Header { return m.header }
func (m *mockTransporter) ReplyHeader() transport.Header   { return m.header }

func ctxWithBearer(token string) context.Context {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return transport.NewServerContext(context.Background(), &mockTransporter{header: &mockHeader{h: h}})
}

// IssueJWT 现在签发 RS256：私钥签发 → 公钥可解析，claims 不变。
func TestIssueJWTRS256Roundtrip(t *testing.T) {
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
		t.Fatalf("configure keys: %v", err)
	}
	db := openTestUserDB(t)
	u := &model.User{ID: 42, Username: "tester", Name: "测试用户", CurrentOrgID: 0}
	token, err := IssueJWT(db, u)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	// 验证方：RS256 + 公钥 + iss/aud 严格校验（与 auth 包 keyFunc 一致）
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(tok *jwt.Token) (interface{}, error) {
		if tok.Method != jwt.SigningMethodRS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return _const.JWTPublicKey(), nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("goalgo"),
		jwt.WithAudience("goalgo-web"),
	)
	if err != nil || !parsed.Valid {
		t.Fatalf("token not valid with RS256 public key: %v", err)
	}
	if parsed.Method != jwt.SigningMethodRS256 {
		t.Fatalf("token method = %v, want RS256", parsed.Method)
	}
	if id := int(claims["userId"].(float64)); id != 42 {
		t.Fatalf("userId = %d, want 42", id)
	}
	if claims["username"] != "tester" || claims["iss"] != "goalgo" {
		t.Fatalf("claims mismatch: %v", claims)
	}
}

// auth 包完整解析路径：RawToken 取原文 + GetCurrentUser 用公钥解析出用户。
func TestAuthParsePayloadRS256(t *testing.T) {
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
		t.Fatalf("configure keys: %v", err)
	}
	db := openTestUserDB(t)
	u := &model.User{ID: 7, Username: "alice", Name: "Alice", CurrentOrgID: 0}
	token, err := IssueJWT(db, u)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	ctx := ctxWithBearer(token)
	if raw := auth.RawToken(ctx); raw != token {
		t.Fatalf("RawToken = %q, want original token", raw)
	}
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		t.Fatal("GetCurrentUser returned nil")
	}
	if pd.UserID != 7 || pd.Username != "alice" || pd.Name != "Alice" {
		t.Fatalf("payload mismatch: %+v", pd)
	}
	// 无 Authorization → RawToken 空、解析 nil
	if raw := auth.RawToken(context.Background()); raw != "" {
		t.Fatalf("RawToken on empty ctx = %q, want empty", raw)
	}
	if pd := auth.GetCurrentUser(context.Background()); pd != nil {
		t.Fatalf("GetCurrentUser on empty ctx = %+v, want nil", pd)
	}
}
