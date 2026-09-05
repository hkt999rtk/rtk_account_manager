package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"github.com/gin-gonic/gin"
	"net/http"
	"regexp"
	"rtk_account_manager/internal/auth"
	"strings"
)

var labUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) testLabAccount(c *gin.Context) {
	if !s.requireTestLab(c) {
		return
	}
	cloud, actor := c.Param("brandCloudId"), currentUserID(c)
	if c.Request.Method == "DELETE" {
		if err := s.testLab.store.CloseLabAccount(c.Request.Context(), actor, cloud, c.Param("accountId")); err != nil {
			writeStoreError(c, err)
			return
		}
		c.Status(204)
		return
	}
	var in struct{}
	if !bindStrict(c, &in) {
		return
	}
	a, err := s.testLab.store.ConsoleLabAccount(c.Request.Context(), actor, cloud)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(200, a)
}

func (s *Server) testLabDevices(c *gin.Context) {
	if !s.requireTestLab(c) {
		return
	}
	actor, cloud := currentUserID(c), c.Param("brandCloudId")
	if c.Request.Method == "GET" {
		if !labUUID.MatchString(cloud) || !labUUID.MatchString(c.Query("product_id")) || !labUUID.MatchString(c.Query("account_id")) {
			writeError(c, 400, "invalid_scope", "Cloud, Product and test account IDs are required")
			return
		}
		if c.Param("deviceId") != "" {
			ready, err := s.testLab.store.LabDeviceReady(c.Request.Context(), actor, cloud, c.Query("product_id"), c.Query("account_id"), c.Param("deviceId"))
			if err != nil {
				writeStoreError(c, err)
				return
			}
			c.JSON(200, gin.H{"runtime_ready": ready})
			return
		}
		limit, offset := pagination(c)
		items, err := s.testLab.store.ListLabDevices(c.Request.Context(), actor, cloud, c.Query("product_id"), c.Query("account_id"), limit, offset)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		c.JSON(200, gin.H{"devices": items, "next_offset": offset + len(items), "has_more": len(items) == limit})
		return
	}
	var in struct {
		Product   string `json:"product_id"`
		Account   string `json:"account_id"`
		Claim     string `json:"claim_token"`
		Operation string `json:"operation_id"`
		Activity  string `json:"activity_id"`
		PublicKey string `json:"clip_public_key"`
	}
	if !bindStrict(c, &in) {
		return
	}
	device, action := c.Param("deviceId"), c.Param("action")
	if !labUUID.MatchString(cloud) || !labUUID.MatchString(in.Product) || !labUUID.MatchString(in.Account) || !labUUID.MatchString(device) {
		writeError(c, 400, "invalid_scope", "Cloud, Product, test account and device IDs are required")
		return
	}
	if action == "provision" {
		block, rest := pem.Decode([]byte(in.PublicKey))
		if block == nil || len(strings.TrimSpace(string(rest))) > 0 || block.Type != "PUBLIC KEY" {
			writeError(c, 400, "invalid_clip_key", "Provide an RSA public key in SPKI PEM format")
			return
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		rsaKey, ok := key.(*rsa.PublicKey)
		if err != nil || !ok || rsaKey.N.BitLen() < 2048 || len(in.PublicKey) > 8192 || len(in.Activity) == 0 || len(in.Activity) > 128 || len(in.Operation) == 0 || len(in.Operation) > 128 {
			writeError(c, 400, "invalid_provision_input", "Check provisioning inputs")
			return
		}
		result, err := s.testLab.store.ProvisionLabDevice(c.Request.Context(), actor, cloud, in.Product, in.Account, device, in.Operation, in.Activity, in.PublicKey)
		if err != nil {
			writeStoreError(c, err)
			return
		}
		c.JSON(202, gin.H{"operation_id": result.Operation.OperationID, "status": result.Operation.Status})
		return
	}
	if action != "grant" && action != "bind" && action != "unbind" {
		c.Status(http.StatusNotFound)
		return
	}
	raw := in.Claim
	if action == "grant" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			c.Status(500)
			return
		}
		raw = hex.EncodeToString(b)
	}
	if (action == "bind" || action == "grant") && len(raw) != 64 {
		writeError(c, 400, "invalid_claim", "A fresh test binding authorization is required")
		return
	}
	err := s.testLab.store.LabBindingAction(c.Request.Context(), actor, cloud, in.Product, in.Account, device, action, auth.HashToken(raw))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	if action == "grant" {
		c.JSON(201, gin.H{"claim_token": raw, "expires_in": 120})
		return
	}
	c.JSON(200, gin.H{"status": action + "_completed"})
}
