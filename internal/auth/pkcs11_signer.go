//go:build pkcs11

package auth

import (
	"crypto"
	"crypto/rsa"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"github.com/miekg/pkcs11"
)

type PKCS11TokenSignerConfig struct {
	ModulePath string
	TokenLabel string
	SlotID     string
	PIN        string
	KeyLabel   string
}

type pkcs11RSASigner struct {
	ctx       *pkcs11.Ctx
	session   pkcs11.SessionHandle
	key       pkcs11.ObjectHandle
	publicKey *rsa.PublicKey
	mu        sync.Mutex
}

func LoadPKCS11TokenSigner(cfg PKCS11TokenSignerConfig) (RS256TokenSigner, error) {
	signer, err := openPKCS11RSASigner(cfg)
	if err != nil {
		return RS256TokenSigner{}, err
	}
	return RS256TokenSigner{Signer: signer, PublicKey: signer.publicKey}, nil
}

func openPKCS11RSASigner(cfg PKCS11TokenSignerConfig) (*pkcs11RSASigner, error) {
	modulePath := strings.TrimSpace(cfg.ModulePath)
	if modulePath == "" {
		return nil, fmt.Errorf("pkcs11 module path is required")
	}
	ctx := pkcs11.New(modulePath)
	if ctx == nil {
		return nil, fmt.Errorf("load pkcs11 module")
	}
	initialized, err := initializePKCS11(ctx)
	if err != nil {
		ctx.Destroy()
		return nil, err
	}
	var session pkcs11.SessionHandle
	cleanup := func() {
		if session != 0 {
			_ = ctx.CloseSession(session)
		}
		if initialized {
			_ = ctx.Finalize()
		}
		ctx.Destroy()
	}
	slotID, err := selectPKCS11Slot(ctx, cfg)
	if err != nil {
		cleanup()
		return nil, err
	}
	session, err = ctx.OpenSession(slotID, pkcs11.CKF_SERIAL_SESSION)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open pkcs11 session: %w", err)
	}
	if err := loginPKCS11User(ctx, session, cfg.PIN); err != nil {
		cleanup()
		return nil, fmt.Errorf("login pkcs11 token: %w", err)
	}
	privateKey, err := findPKCS11Key(ctx, session, pkcs11.CKO_PRIVATE_KEY, cfg.KeyLabel)
	if err != nil {
		cleanup()
		return nil, err
	}
	publicHandle, err := findPKCS11Key(ctx, session, pkcs11.CKO_PUBLIC_KEY, cfg.KeyLabel)
	if err != nil {
		cleanup()
		return nil, err
	}
	publicKey, err := readPKCS11RSAPublicKey(ctx, session, publicHandle)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &pkcs11RSASigner{ctx: ctx, session: session, key: privateKey, publicKey: publicKey}, nil
}

func initializePKCS11(ctx *pkcs11.Ctx) (bool, error) {
	if err := ctx.Initialize(); err != nil {
		if isPKCS11Error(err, pkcs11.CKR_CRYPTOKI_ALREADY_INITIALIZED) {
			return false, nil
		}
		return false, fmt.Errorf("initialize pkcs11 module: %w", err)
	}
	return true, nil
}

func loginPKCS11User(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, pin string) error {
	if err := ctx.Login(session, pkcs11.CKU_USER, pin); err != nil {
		if isPKCS11Error(err, pkcs11.CKR_USER_ALREADY_LOGGED_IN) {
			return nil
		}
		return err
	}
	return nil
}

func isPKCS11Error(err error, code uint) bool {
	pkcs11Err, ok := err.(pkcs11.Error)
	return ok && pkcs11Err == pkcs11.Error(code)
}

func selectPKCS11Slot(ctx *pkcs11.Ctx, cfg PKCS11TokenSignerConfig) (uint, error) {
	if raw := strings.TrimSpace(cfg.SlotID); raw != "" {
		slotID, err := strconv.ParseUint(raw, 10, 0)
		if err != nil {
			return 0, fmt.Errorf("parse pkcs11 slot id: %w", err)
		}
		return uint(slotID), nil
	}
	tokenLabel := strings.TrimSpace(cfg.TokenLabel)
	if tokenLabel == "" {
		return 0, fmt.Errorf("pkcs11 token label or slot id is required")
	}
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		return 0, fmt.Errorf("list pkcs11 slots: %w", err)
	}
	for _, slotID := range slots {
		info, err := ctx.GetTokenInfo(slotID)
		if err == nil && strings.TrimSpace(info.Label) == tokenLabel {
			return slotID, nil
		}
	}
	return 0, fmt.Errorf("pkcs11 token %q not found", tokenLabel)
}

func findPKCS11Key(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, class uint, keyLabel string) (pkcs11.ObjectHandle, error) {
	keyLabel = strings.TrimSpace(keyLabel)
	if keyLabel == "" {
		return 0, fmt.Errorf("pkcs11 key label is required")
	}
	if err := ctx.FindObjectsInit(session, []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, class),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
	}); err != nil {
		return 0, fmt.Errorf("find pkcs11 key: %w", err)
	}
	objects, _, err := ctx.FindObjects(session, 2)
	finalErr := ctx.FindObjectsFinal(session)
	if err != nil {
		return 0, fmt.Errorf("find pkcs11 key: %w", err)
	}
	if finalErr != nil {
		return 0, fmt.Errorf("finish pkcs11 key search: %w", finalErr)
	}
	if len(objects) == 0 {
		return 0, fmt.Errorf("pkcs11 key %q not found", keyLabel)
	}
	if len(objects) > 1 {
		return 0, fmt.Errorf("pkcs11 key %q is ambiguous", keyLabel)
	}
	return objects[0], nil
}

func readPKCS11RSAPublicKey(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, key pkcs11.ObjectHandle) (*rsa.PublicKey, error) {
	attrs, err := ctx.GetAttributeValue(session, key, []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_PUBLIC_EXPONENT, nil),
	})
	if err != nil {
		return nil, fmt.Errorf("read pkcs11 public key: %w", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(attrs[0].Value),
		E: int(new(big.Int).SetBytes(attrs[1].Value).Int64()),
	}, nil
}

func (s *pkcs11RSASigner) Public() crypto.PublicKey {
	return s.publicKey
}

func (s *pkcs11RSASigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts == nil || opts.HashFunc() != crypto.SHA256 {
		return nil, fmt.Errorf("pkcs11 jwt signer requires SHA-256 digest")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.SignInit(s.session, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS, nil)}, s.key); err != nil {
		return nil, fmt.Errorf("pkcs11 jwt sign init: %w", err)
	}
	return s.ctx.Sign(s.session, rsaDigestInfo(digest))
}

func rsaDigestInfo(digest []byte) []byte {
	prefix := []byte{0x30, 0x31, 0x30, 0x0d, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x01, 0x05, 0x00, 0x04, 0x20}
	out := make([]byte, 0, len(prefix)+len(digest))
	out = append(out, prefix...)
	out = append(out, digest...)
	return out
}
