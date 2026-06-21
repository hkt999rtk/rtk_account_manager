package usercache

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type RedisCache struct {
	cfg      Config
	dial     func(context.Context, string) (net.Conn, error)
	command  func(context.Context, ...string) (any, error)
	deadline time.Duration
}

type platformAuthProjection struct {
	User         model.User `json:"user"`
	PasswordHash string     `json:"password_hash"`
}

type brandLoginProjection struct {
	BrandCloud     model.Organization     `json:"brand_cloud"`
	User           model.User             `json:"user"`
	BrandCloudUser model.BrandCloudUser   `json:"brand_cloud_user"`
	Member         model.BrandCloudMember `json:"member"`
	PasswordHash   string                 `json:"password_hash"`
}

type endLoginProjection struct {
	EndUser      model.EndUser `json:"end_user"`
	PasswordHash string        `json:"password_hash"`
}

type scanResult struct {
	cursor string
	keys   []string
}

func NewRedisCache(cfg Config) *RedisCache {
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), ":")
	if prefix == "" {
		prefix = "account_manager:user"
	}
	cfg.Prefix = prefix
	cache := &RedisCache{
		cfg:      cfg,
		deadline: 2 * time.Second,
	}
	cache.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimSpace(addr))
	}
	cache.command = cache.redisCommand
	return cache
}

func (c *RedisCache) GetPlatformUser(ctx context.Context, userID string) (model.User, bool, error) {
	var user model.User
	ok, err := c.getJSON(ctx, c.platformUserKey(userID), &user)
	return user, ok, err
}

func (c *RedisCache) GetPlatformUserIDByEmail(ctx context.Context, email string) (string, bool, error) {
	value, err := c.getString(ctx, c.platformEmailKey(email))
	if err != nil || value == "" {
		return "", false, err
	}
	return value, true, nil
}

func (c *RedisCache) GetPlatformAuth(ctx context.Context, userID string) (model.User, string, bool, error) {
	var projection platformAuthProjection
	ok, err := c.getJSON(ctx, c.platformAuthKey(userID), &projection)
	if err != nil || !ok {
		return model.User{}, "", ok, err
	}
	return projection.User, projection.PasswordHash, true, nil
}

func (c *RedisCache) PutPlatformUser(ctx context.Context, user model.User) error {
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return errors.New("missing platform user id")
	}
	if err := c.setJSON(ctx, c.platformUserKey(userID), user); err != nil {
		return err
	}
	return c.setString(ctx, c.platformEmailKey(user.Email), userID)
}

func (c *RedisCache) PutPlatformAuth(ctx context.Context, user model.User, hash string) error {
	if err := c.PutPlatformUser(ctx, user); err != nil {
		return err
	}
	return c.setJSON(ctx, c.platformAuthKey(user.ID), platformAuthProjection{User: user, PasswordHash: hash})
}

func (c *RedisCache) DeletePlatformUser(ctx context.Context, userID string) error {
	user, ok, err := c.GetPlatformUser(ctx, userID)
	if err != nil {
		return err
	}
	keys := []string{c.platformUserKey(userID), c.platformAuthKey(userID)}
	if ok {
		keys = append(keys, c.platformEmailKey(user.Email))
	}
	_, err = c.command(ctx, append([]string{"DEL"}, keys...)...)
	return err
}

func (c *RedisCache) FlushPlatformAuth(ctx context.Context) error {
	return c.deleteByPattern(ctx, c.platformAuthKey("*"))
}

func (c *RedisCache) GetBrandCloudUser(ctx context.Context, userID string) (model.BrandCloudUser, bool, error) {
	var user model.BrandCloudUser
	ok, err := c.getJSON(ctx, c.brandUserKey(userID), &user)
	return user, ok, err
}

func (c *RedisCache) GetBrandCloudUserIDByTenantEmail(ctx context.Context, tenantSlug, email string) (string, bool, error) {
	value, err := c.getString(ctx, c.brandEmailKey(tenantSlug, email))
	if err != nil || value == "" {
		return "", false, err
	}
	return value, true, nil
}

func (c *RedisCache) GetBrandCloudLogin(ctx context.Context, userID string) (store.BrandCloudLoginResult, bool, error) {
	var projection brandLoginProjection
	ok, err := c.getJSON(ctx, c.brandAuthKey(userID), &projection)
	if err != nil || !ok {
		return store.BrandCloudLoginResult{}, ok, err
	}
	return store.BrandCloudLoginResult{
		BrandCloud:     projection.BrandCloud,
		User:           projection.User,
		BrandCloudUser: projection.BrandCloudUser,
		Member:         projection.Member,
		PasswordHash:   projection.PasswordHash,
	}, true, nil
}

func (c *RedisCache) PutBrandCloudUser(ctx context.Context, user model.BrandCloudUser) error {
	if strings.TrimSpace(user.ID) == "" {
		return errors.New("missing brand cloud user id")
	}
	return c.setJSON(ctx, c.brandUserKey(user.ID), user)
}

func (c *RedisCache) PutBrandCloudLogin(ctx context.Context, tenantSlug string, result store.BrandCloudLoginResult) error {
	user := result.BrandCloudUser
	if err := c.PutBrandCloudUser(ctx, user); err != nil {
		return err
	}
	if err := c.setString(ctx, c.brandEmailKey(tenantSlug, user.Email), user.ID); err != nil {
		return err
	}
	return c.setJSON(ctx, c.brandAuthKey(user.ID), brandLoginProjection{
		BrandCloud:     result.BrandCloud,
		User:           result.User,
		BrandCloudUser: result.BrandCloudUser,
		Member:         result.Member,
		PasswordHash:   result.PasswordHash,
	})
}

func (c *RedisCache) DeleteBrandCloudUser(ctx context.Context, userID string) error {
	user, ok, err := c.GetBrandCloudUser(ctx, userID)
	if err != nil {
		return err
	}
	keys := []string{c.brandUserKey(userID), c.brandAuthKey(userID)}
	if ok {
		if err := c.deleteByPattern(ctx, c.cfg.Prefix+":brand_cloud:email:*:"+escapeKey(normalizeEmail(user.Email))); err != nil {
			return err
		}
	}
	_, err = c.command(ctx, append([]string{"DEL"}, keys...)...)
	return err
}

func (c *RedisCache) GetEndUser(ctx context.Context, userID string) (model.EndUser, bool, error) {
	var user model.EndUser
	ok, err := c.getJSON(ctx, c.endUserKey(userID), &user)
	return user, ok, err
}

func (c *RedisCache) GetEndUserIDByEmail(ctx context.Context, email string) (string, bool, error) {
	value, err := c.getString(ctx, c.endEmailKey(email))
	if err != nil || value == "" {
		return "", false, err
	}
	return value, true, nil
}

func (c *RedisCache) GetEndUserLogin(ctx context.Context, userID string) (store.EndUserLoginResult, bool, error) {
	var projection endLoginProjection
	ok, err := c.getJSON(ctx, c.endAuthKey(userID), &projection)
	if err != nil || !ok {
		return store.EndUserLoginResult{}, ok, err
	}
	return store.EndUserLoginResult{
		EndUser:      projection.EndUser,
		PasswordHash: projection.PasswordHash,
	}, true, nil
}

func (c *RedisCache) PutEndUser(ctx context.Context, user model.EndUser) error {
	if strings.TrimSpace(user.ID) == "" {
		return errors.New("missing end user id")
	}
	if err := c.setJSON(ctx, c.endUserKey(user.ID), user); err != nil {
		return err
	}
	return c.setString(ctx, c.endEmailKey(user.PrimaryEmail), user.ID)
}

func (c *RedisCache) PutEndUserLogin(ctx context.Context, result store.EndUserLoginResult) error {
	if err := c.PutEndUser(ctx, result.EndUser); err != nil {
		return err
	}
	return c.setJSON(ctx, c.endAuthKey(result.EndUser.ID), endLoginProjection{
		EndUser:      result.EndUser,
		PasswordHash: result.PasswordHash,
	})
}

func (c *RedisCache) DeleteEndUser(ctx context.Context, userID string) error {
	user, ok, err := c.GetEndUser(ctx, userID)
	if err != nil {
		return err
	}
	keys := []string{c.endUserKey(userID), c.endAuthKey(userID)}
	if ok {
		keys = append(keys, c.endEmailKey(user.PrimaryEmail))
	}
	_, err = c.command(ctx, append([]string{"DEL"}, keys...)...)
	return err
}

func (c *RedisCache) getJSON(ctx context.Context, key string, dest any) (bool, error) {
	payload, err := c.getString(ctx, key)
	if err != nil || payload == "" {
		return false, err
	}
	if err := json.Unmarshal([]byte(payload), dest); err != nil {
		return false, err
	}
	return true, nil
}

func (c *RedisCache) getString(ctx context.Context, key string) (string, error) {
	payload, err := c.command(ctx, "GET", key)
	if err != nil || payload == nil {
		return "", err
	}
	text, ok := payload.(string)
	if !ok {
		return "", fmt.Errorf("unexpected GET response %T", payload)
	}
	return text, nil
}

func (c *RedisCache) setJSON(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.setString(ctx, key, string(raw))
}

func (c *RedisCache) setString(ctx context.Context, key string, value string) error {
	_, err := c.command(ctx, "SET", key, value)
	return err
}

func (c *RedisCache) deleteByPattern(ctx context.Context, pattern string) error {
	cursor := "0"
	for {
		resp, err := c.command(ctx, "SCAN", cursor, "MATCH", pattern, "COUNT", "100")
		if err != nil {
			return err
		}
		scan, ok := resp.(scanResult)
		if !ok {
			return fmt.Errorf("unexpected SCAN response %T", resp)
		}
		if len(scan.keys) > 0 {
			if _, err := c.command(ctx, append([]string{"DEL"}, scan.keys...)...); err != nil {
				return err
			}
		}
		if scan.cursor == "0" {
			return nil
		}
		cursor = scan.cursor
	}
}

func (c *RedisCache) platformUserKey(userID string) string {
	return c.cfg.Prefix + ":platform:id:" + escapeKey(userID)
}

func (c *RedisCache) platformEmailKey(email string) string {
	return c.cfg.Prefix + ":platform:email:" + escapeKey(normalizeEmail(email))
}

func (c *RedisCache) platformAuthKey(userID string) string {
	return c.cfg.Prefix + ":platform:auth:" + escapeKey(userID)
}

func (c *RedisCache) brandUserKey(userID string) string {
	return c.cfg.Prefix + ":brand_cloud:id:" + escapeKey(userID)
}

func (c *RedisCache) brandEmailKey(tenantSlug, email string) string {
	return c.cfg.Prefix + ":brand_cloud:email:" + escapeKey(normalizeTenantSlug(tenantSlug)) + ":" + escapeKey(normalizeEmail(email))
}

func (c *RedisCache) brandAuthKey(userID string) string {
	return c.cfg.Prefix + ":brand_cloud:auth:" + escapeKey(userID)
}

func (c *RedisCache) endUserKey(userID string) string {
	return c.cfg.Prefix + ":end_user:id:" + escapeKey(userID)
}

func (c *RedisCache) endEmailKey(email string) string {
	return c.cfg.Prefix + ":end_user:email:" + escapeKey(normalizeEmail(email))
}

func (c *RedisCache) endAuthKey(userID string) string {
	return c.cfg.Prefix + ":end_user:auth:" + escapeKey(userID)
}

func (c *RedisCache) redisCommand(ctx context.Context, args ...string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("empty redis command")
	}
	addr := strings.TrimSpace(c.cfg.Addr)
	if addr == "" {
		return nil, errors.New("missing redis address")
	}
	deadlineTimeout := c.deadline
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < deadlineTimeout {
			deadlineTimeout = remaining
		}
	}
	dialCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok && deadlineTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, deadlineTimeout)
	}
	defer cancel()
	conn, err := c.dial(dialCtx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadlineTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(deadlineTimeout))
	}
	if err := writeRESPCommand(conn, args...); err != nil {
		return nil, err
	}
	return readRESP(bufio.NewReader(conn))
}

func writeRESPCommand(w io.Writer, args ...string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return nil
}

func readRESP(r *bufio.Reader) (any, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	switch prefix {
	case '+':
		return line, nil
	case '-':
		return nil, errors.New(line)
	case ':':
		return strconv.ParseInt(line, 10, 64)
	case '$':
		size, err := strconv.Atoi(line)
		if err != nil {
			return nil, err
		}
		if size < 0 {
			return nil, nil
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:size]), nil
	case '*':
		count, err := strconv.Atoi(line)
		if err != nil {
			return nil, err
		}
		if count < 0 {
			return nil, nil
		}
		values := make([]any, 0, count)
		for i := 0; i < count; i++ {
			value, err := readRESP(r)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if count == 2 {
			cursor, cursorOK := values[0].(string)
			rawKeys, keysOK := values[1].([]string)
			if cursorOK && keysOK {
				return scanResult{cursor: cursor, keys: rawKeys}, nil
			}
			if cursorOK {
				if list, ok := values[1].([]any); ok {
					keys := make([]string, 0, len(list))
					for _, item := range list {
						if key, ok := item.(string); ok {
							keys = append(keys, key)
						}
					}
					return scanResult{cursor: cursor, keys: keys}, nil
				}
			}
		}
		stringsOnly := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return values, nil
			}
			stringsOnly = append(stringsOnly, text)
		}
		return stringsOnly, nil
	default:
		return nil, fmt.Errorf("unsupported RESP prefix %q", prefix)
	}
}

func escapeKey(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, " ", "%20")
	value = strings.ReplaceAll(value, ":", "%3A")
	return value
}
