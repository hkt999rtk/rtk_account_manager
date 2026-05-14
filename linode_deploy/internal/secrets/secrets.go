package secrets

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Values map[string]string

func Load(path string) (Values, error) {
	if path == "" {
		return Values{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}
	defer f.Close()
	vals := Values{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return vals, nil
}

func (v Values) Get(key string) string {
	if v == nil {
		return ""
	}
	return v[key]
}

func (v Values) Require(keys ...string) error {
	missing := []string{}
	for _, key := range keys {
		if strings.TrimSpace(v.Get(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required secrets: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (v Values) Redact(s string) string {
	values := []string{}
	for key, value := range v {
		if !secretLikeKey(key) || len(value) < 4 {
			continue
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		s = strings.ReplaceAll(s, value, "<redacted>")
	}
	return s
}

func secretLikeKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "SECRET") ||
		strings.Contains(key, "TOKEN") ||
		strings.Contains(key, "DSN")
}
