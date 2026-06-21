package usercache

import (
	"bufio"
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestRedisCachePlatformAuthUsesExpectedKeysWithoutTTL(t *testing.T) {
	cache := NewRedisCache(Config{Prefix: "account_manager:user"})
	commands := [][]string{}
	cache.command = func(_ context.Context, args ...string) (any, error) {
		commands = append(commands, append([]string(nil), args...))
		return "OK", nil
	}

	user := testUser("user-1", "Owner@Example.com")
	if err := cache.PutPlatformAuth(context.Background(), user, "hash-1"); err != nil {
		t.Fatal(err)
	}

	if len(commands) != 3 {
		t.Fatalf("expected profile, email index, and auth SET commands, got %#v", commands)
	}
	wantKeys := []string{
		"account_manager:user:platform:id:user-1",
		"account_manager:user:platform:email:owner@example.com",
		"account_manager:user:platform:auth:user-1",
	}
	for i, want := range wantKeys {
		if commands[i][0] != "SET" || commands[i][1] != want {
			t.Fatalf("command %d = %#v, want SET %s", i, commands[i], want)
		}
		if len(commands[i]) != 3 {
			t.Fatalf("command %d should not include TTL args: %#v", i, commands[i])
		}
	}
}

func TestRedisCacheFlushPlatformAuthScansAndDeletesOnlyAuthKeys(t *testing.T) {
	cache := NewRedisCache(Config{Prefix: "account_manager:user"})
	commands := [][]string{}
	cache.command = func(_ context.Context, args ...string) (any, error) {
		commands = append(commands, append([]string(nil), args...))
		if args[0] == "SCAN" {
			return scanResult{cursor: "0", keys: []string{
				"account_manager:user:platform:auth:user-1",
				"account_manager:user:platform:auth:user-2",
			}}, nil
		}
		return int64(2), nil
	}

	if err := cache.FlushPlatformAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected SCAN and DEL, got %#v", commands)
	}
	if commands[0][0] != "SCAN" || commands[0][2] != "MATCH" || commands[0][3] != "account_manager:user:platform:auth:*" {
		t.Fatalf("unexpected scan command: %#v", commands[0])
	}
	if commands[1][0] != "DEL" || len(commands[1]) != 3 {
		t.Fatalf("unexpected del command: %#v", commands[1])
	}
}

func TestRedisCacheRoundTripsPlatformBrandAndEndUserProjections(t *testing.T) {
	ctx := context.Background()
	cache := NewRedisCache(Config{Prefix: "account_manager:user"})
	installMemoryRedis(cache)

	platformUser := testUser("user-1", "Owner@Example.com")
	if err := cache.PutPlatformAuth(ctx, platformUser, "platform-hash"); err != nil {
		t.Fatal(err)
	}
	gotPlatform, hash, ok, err := cache.GetPlatformAuth(ctx, "user-1")
	if err != nil || !ok {
		t.Fatalf("expected platform auth hit, ok=%t err=%v", ok, err)
	}
	if gotPlatform.ID != "user-1" || hash != "platform-hash" {
		t.Fatalf("unexpected platform auth: %+v %q", gotPlatform, hash)
	}
	platformID, ok, err := cache.GetPlatformUserIDByEmail(ctx, " owner@example.com ")
	if err != nil || !ok || platformID != "user-1" {
		t.Fatalf("unexpected platform email index: %q %t %v", platformID, ok, err)
	}
	if err := cache.DeletePlatformUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.GetPlatformUser(ctx, "user-1"); err != nil || ok {
		t.Fatalf("expected deleted platform user miss, ok=%t err=%v", ok, err)
	}

	brandResult := store.BrandCloudLoginResult{
		BrandCloudUser: model.BrandCloudUser{ID: "brand-user-1", BrandCloudID: "brand-1", Email: "operator@example.com"},
		PasswordHash:   "brand-hash",
	}
	if err := cache.PutBrandCloudLogin(ctx, "ACME", brandResult); err != nil {
		t.Fatal(err)
	}
	brandID, ok, err := cache.GetBrandCloudUserIDByTenantEmail(ctx, "acme", "operator@example.com")
	if err != nil || !ok || brandID != "brand-user-1" {
		t.Fatalf("unexpected brand email index: %q %t %v", brandID, ok, err)
	}
	gotBrand, ok, err := cache.GetBrandCloudLogin(ctx, "brand-user-1")
	if err != nil || !ok || gotBrand.PasswordHash != "brand-hash" {
		t.Fatalf("unexpected brand login: %+v %t %v", gotBrand, ok, err)
	}
	if err := cache.DeleteBrandCloudUser(ctx, "brand-user-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.GetBrandCloudLogin(ctx, "brand-user-1"); err != nil || ok {
		t.Fatalf("expected deleted brand login miss, ok=%t err=%v", ok, err)
	}

	endResult := store.EndUserLoginResult{
		EndUser:      model.EndUser{ID: "end-user-1", PrimaryEmail: "consumer@example.com", Status: "active"},
		PasswordHash: "end-hash",
	}
	if err := cache.PutEndUserLogin(ctx, endResult); err != nil {
		t.Fatal(err)
	}
	endID, ok, err := cache.GetEndUserIDByEmail(ctx, " Consumer@Example.com ")
	if err != nil || !ok || endID != "end-user-1" {
		t.Fatalf("unexpected end email index: %q %t %v", endID, ok, err)
	}
	gotEnd, ok, err := cache.GetEndUserLogin(ctx, "end-user-1")
	if err != nil || !ok || gotEnd.PasswordHash != "end-hash" {
		t.Fatalf("unexpected end login: %+v %t %v", gotEnd, ok, err)
	}
	if err := cache.DeleteEndUser(ctx, "end-user-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.GetEndUser(ctx, "end-user-1"); err != nil || ok {
		t.Fatalf("expected deleted end user miss, ok=%t err=%v", ok, err)
	}
}

func TestRedisRESPHelpers(t *testing.T) {
	var out strings.Builder
	if err := writeRESPCommand(&out, "SET", "key:1", "value"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "*3\r\n$3\r\nSET\r\n$5\r\nkey:1\r\n$5\r\nvalue\r\n" {
		t.Fatalf("unexpected RESP command: %q", got)
	}

	cases := []struct {
		raw  string
		want any
	}{
		{"+OK\r\n", "OK"},
		{":2\r\n", int64(2)},
		{"$5\r\nhello\r\n", "hello"},
		{"$-1\r\n", nil},
		{"*2\r\n$1\r\n0\r\n*2\r\n$5\r\nkey-1\r\n$5\r\nkey-2\r\n", scanResult{cursor: "0", keys: []string{"key-1", "key-2"}}},
	}
	for _, tc := range cases {
		got, err := readRESP(bufio.NewReader(strings.NewReader(tc.raw)))
		if err != nil {
			t.Fatalf("readRESP(%q): %v", tc.raw, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("readRESP(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
	if _, err := readRESP(bufio.NewReader(strings.NewReader("-ERR bad\r\n"))); err == nil {
		t.Fatal("expected RESP error")
	}
}

func TestRedisCommandRoundTripAndValidationErrors(t *testing.T) {
	cache := NewRedisCache(Config{})
	if _, err := cache.redisCommand(context.Background()); err == nil {
		t.Fatal("expected empty command error")
	}
	if _, err := cache.redisCommand(context.Background(), "PING"); err == nil {
		t.Fatal("expected missing address error")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer conn.Close()
		args, err := readRESP(bufio.NewReader(conn))
		if err != nil {
			done <- nil
			return
		}
		_, _ = conn.Write([]byte("+OK\r\n"))
		values, _ := args.([]string)
		done <- values
	}()

	cache = NewRedisCache(Config{Addr: ln.Addr().String()})
	got, err := cache.redisCommand(context.Background(), "SET", "key", "value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "OK" {
		t.Fatalf("unexpected response: %#v", got)
	}
	if args := <-done; !reflect.DeepEqual(args, []string{"SET", "key", "value"}) {
		t.Fatalf("unexpected server args: %#v", args)
	}
}

func TestRedisCacheValidationAndDecodeErrors(t *testing.T) {
	ctx := context.Background()
	cache := NewRedisCache(Config{Prefix: "account_manager:user"})
	values := installMemoryRedis(cache)

	if err := cache.PutPlatformUser(ctx, model.User{}); err == nil {
		t.Fatal("expected missing platform user id error")
	}
	if err := cache.PutBrandCloudUser(ctx, model.BrandCloudUser{}); err == nil {
		t.Fatal("expected missing brand cloud user id error")
	}
	if err := cache.PutEndUser(ctx, model.EndUser{}); err == nil {
		t.Fatal("expected missing end user id error")
	}

	values["account_manager:user:platform:id:user-1"] = "{"
	if _, ok, err := cache.GetPlatformUser(ctx, "user-1"); err == nil || ok {
		t.Fatalf("expected decode error and miss, ok=%t err=%v", ok, err)
	}
	cache.command = func(context.Context, ...string) (any, error) {
		return int64(1), nil
	}
	if _, ok, err := cache.GetPlatformUserIDByEmail(ctx, "owner@example.com"); err == nil || ok {
		t.Fatalf("expected unexpected response error, ok=%t err=%v", ok, err)
	}
}

func installMemoryRedis(cache *RedisCache) map[string]string {
	values := map[string]string{}
	cache.command = func(_ context.Context, args ...string) (any, error) {
		switch args[0] {
		case "GET":
			value, ok := values[args[1]]
			if !ok {
				return nil, nil
			}
			return value, nil
		case "SET":
			values[args[1]] = args[2]
			return "OK", nil
		case "DEL":
			for _, key := range args[1:] {
				delete(values, key)
			}
			return int64(len(args) - 1), nil
		case "SCAN":
			pattern := strings.TrimSuffix(args[3], "*")
			keys := []string{}
			for key := range values {
				if strings.HasPrefix(key, pattern) {
					keys = append(keys, key)
				}
			}
			return scanResult{cursor: "0", keys: keys}, nil
		default:
			return nil, nil
		}
	}
	return values
}
