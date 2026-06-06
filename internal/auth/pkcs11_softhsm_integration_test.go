//go:build pkcs11

package auth

import (
	"crypto/rsa"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/pkcs11"
)

func TestLoadPKCS11TokenSignerWithSoftHSM(t *testing.T) {
	modulePath := os.Getenv("ACCOUNT_MANAGER_TEST_PKCS11_MODULE_PATH")
	if modulePath == "" {
		t.Skip("ACCOUNT_MANAGER_TEST_PKCS11_MODULE_PATH is not set")
	}

	const (
		tokenLabel = "rtk-account-jwt"
		soPIN      = "12345678"
		userPIN    = "1234"
		accessKey  = "jwt-access-key"
		refreshKey = "jwt-refresh-key"
	)
	t.Setenv("SOFTHSM2_CONF", writeSoftHSMConfig(t))
	initSoftHSMToken(t, modulePath, tokenLabel, soPIN, userPIN)
	generateSoftHSMRSAKeyPair(t, modulePath, tokenLabel, userPIN, accessKey)
	generateSoftHSMRSAKeyPair(t, modulePath, tokenLabel, userPIN, refreshKey)

	accessSigner, err := LoadPKCS11TokenSigner(PKCS11TokenSignerConfig{
		ModulePath: modulePath,
		TokenLabel: tokenLabel,
		PIN:        userPIN,
		KeyLabel:   accessKey,
	})
	if err != nil {
		t.Fatalf("LoadPKCS11TokenSigner(access) error = %v", err)
	}
	refreshSigner, err := LoadPKCS11TokenSigner(PKCS11TokenSignerConfig{
		ModulePath: modulePath,
		TokenLabel: tokenLabel,
		PIN:        userPIN,
		KeyLabel:   refreshKey,
	})
	if err != nil {
		t.Fatalf("LoadPKCS11TokenSigner(refresh) error = %v", err)
	}

	svc := NewServiceWithSigners(accessSigner, refreshSigner, time.Minute, time.Hour)
	accessToken, _, err := svc.IssueAccessToken("user-1")
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}
	if _, err := svc.ParseAccessToken(accessToken); err != nil {
		t.Fatalf("ParseAccessToken() error = %v", err)
	}
	if _, err := svc.ParseRefreshToken(accessToken); err == nil {
		t.Fatal("expected access token to fail refresh-token parsing")
	}
}

func writeSoftHSMConfig(t *testing.T) string {
	t.Helper()

	tokenDir := filepath.Join(t.TempDir(), "tokens")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "softhsm2.conf")
	contents := []byte("directories.tokendir = " + tokenDir + "\nobjectstore.backend = file\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func initSoftHSMToken(t *testing.T, modulePath, tokenLabel, soPIN, userPIN string) string {
	t.Helper()

	ctx := pkcs11.New(modulePath)
	if err := ctx.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	defer func() {
		_ = ctx.Finalize()
		ctx.Destroy()
	}()
	slots, err := ctx.GetSlotList(false)
	if err != nil {
		t.Fatalf("GetSlotList() error = %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("SoftHSM returned no slots")
	}
	slotID := slots[0]
	if err := ctx.InitToken(slotID, soPIN, tokenLabel); err != nil {
		t.Fatalf("InitToken() error = %v", err)
	}
	session, err := ctx.OpenSession(slotID, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	defer ctx.CloseSession(session)
	if err := ctx.Login(session, pkcs11.CKU_SO, soPIN); err != nil {
		t.Fatalf("Login(SO) error = %v", err)
	}
	if err := ctx.InitPIN(session, userPIN); err != nil {
		t.Fatalf("InitPIN() error = %v", err)
	}
	_ = ctx.Logout(session)
	_ = ctx.CloseSession(session)
	return findSoftHSMTokenSlot(t, ctx, tokenLabel)
}

func findSoftHSMTokenSlot(t *testing.T, ctx *pkcs11.Ctx, tokenLabel string) string {
	t.Helper()

	slots, err := ctx.GetSlotList(true)
	if err != nil {
		t.Fatalf("GetSlotList(initialized) error = %v", err)
	}
	for _, slotID := range slots {
		info, err := ctx.GetTokenInfo(slotID)
		if err == nil && strings.TrimSpace(info.Label) == tokenLabel {
			return strconv.FormatUint(uint64(slotID), 10)
		}
	}
	t.Fatalf("initialized SoftHSM token %q not found", tokenLabel)
	return ""
}

func generateSoftHSMRSAKeyPair(t *testing.T, modulePath, tokenLabel, userPIN, keyLabel string) *rsa.PublicKey {
	t.Helper()

	ctx := pkcs11.New(modulePath)
	if err := ctx.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	session := pkcs11.SessionHandle(0)
	defer func() {
		if session != 0 {
			_ = ctx.CloseSession(session)
		}
		_ = ctx.Finalize()
		ctx.Destroy()
	}()
	slotID := findSoftHSMTokenSlot(t, ctx, tokenLabel)
	parsedSlotID, ok := new(big.Int).SetString(slotID, 10)
	if !ok {
		t.Fatalf("invalid slot id %q", slotID)
	}
	var err error
	session, err = ctx.OpenSession(uint(parsedSlotID.Uint64()), pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	if err := ctx.Login(session, pkcs11.CKU_USER, userPIN); err != nil {
		t.Fatalf("Login(USER) error = %v", err)
	}
	public, _, err := ctx.GenerateKeyPair(
		session,
		[]*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS_KEY_PAIR_GEN, nil)},
		[]*pkcs11.Attribute{
			pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PUBLIC_KEY),
			pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_RSA),
			pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
			pkcs11.NewAttribute(pkcs11.CKA_VERIFY, true),
			pkcs11.NewAttribute(pkcs11.CKA_MODULUS_BITS, 2048),
			pkcs11.NewAttribute(pkcs11.CKA_PUBLIC_EXPONENT, []byte{1, 0, 1}),
			pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
		},
		[]*pkcs11.Attribute{
			pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PRIVATE_KEY),
			pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_RSA),
			pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
			pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true),
			pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),
			pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, false),
			pkcs11.NewAttribute(pkcs11.CKA_SIGN, true),
			pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
		},
	)
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}
	attrs, err := ctx.GetAttributeValue(session, public, []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_PUBLIC_EXPONENT, nil),
	})
	if err != nil {
		t.Fatalf("GetAttributeValue() error = %v", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(attrs[0].Value),
		E: int(new(big.Int).SetBytes(attrs[1].Value).Int64()),
	}
}
