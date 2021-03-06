package ldap

import (
	"fmt"
	"strings"

	"gopkg.in/ldap.v0"
)

type Config struct {
	URL      string
	BaseDN   string
	BindDN   string
	BindPass string
}

type Conn interface {
	Close() error
	Search(search *Search) ([]Result, error)
}

type Search struct {
	Filter string
	Attrs  []string
}

type Result struct {
	DN    string
	Attrs []Attr
}

type Attr struct {
	Name   string
	Values []string
}

func (r *Result) Values(name string) []string {
	for _, attr := range r.Attrs {
		if attr.Name == name {
			return attr.Values
		}
	}
	return nil
}

func (r *Result) Value(name string) string {
	values := r.Values(name)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

type ldapConn struct {
	conn   *ldap.Conn
	baseDN string
}

var TestDial func(*Config) (Conn, error)

func Dial(config *Config) (Conn, error) {
	if TestDial != nil {
		return TestDial(config)
	}
	var conn *ldap.Conn
	var err error
	if strings.HasPrefix(config.URL, "ldaps://") {
		conn, err = ldap.DialTLS("tcp", config.URL[8:], nil)
	} else if strings.HasPrefix(config.URL, "ldap://") {
		conn, err = ldap.Dial("tcp", config.URL[7:])
	} else {
		conn, err = ldap.Dial("tcp", config.URL)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot dial LDAP server: %v", err)
	}
	if err := conn.Bind(config.BindDN, config.BindPass); err != nil {
		conn.Close()
		s := strings.Replace(err.Error(), config.BindPass, "********", -1)
		return nil, fmt.Errorf("cannot bind to LDAP server: %s", s)
	}
	return &ldapConn{conn, config.BaseDN}, nil
}

func (c *ldapConn) Close() error {
	c.conn.Close()
	return nil
}

func (c *ldapConn) Search(s *Search) ([]Result, error) {
	search := ldap.NewSearchRequest(
		c.baseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		s.Filter,
		s.Attrs,
		nil,
	)
	result, err := c.conn.Search(search)
	if err != nil {
		return nil, err
	}
	r := make([]Result, len(result.Entries))
	for ei, entry := range result.Entries {
		ri := &r[ei]
		ri.DN = entry.DN
		ri.Attrs = make([]Attr, len(entry.Attributes))
		for ai, attr := range entry.Attributes {
			ri.Attrs[ai] = Attr{attr.Name, attr.Values}
		}
	}
	return r, nil
}

var hex = "0123456789abcdef"

func mustEscape(c byte) bool {
	return c > 0x7f || c == '(' || c == ')' || c == '\\' || c == '*' || c == 0
}

// EscapeFilter escapes from the provided LDAP filter string the special
// characters in the set `()*\` and those out of the range 0 < c < 0x80,
// as defined in RFC4515.
func EscapeFilter(filter string) string {
	escape := 0
	for i := 0; i < len(filter); i++ {
		if mustEscape(filter[i]) {
			escape++
		}
	}
	if escape == 0 {
		return filter
	}
	buf := make([]byte, len(filter)+escape*2)
	for i, j := 0, 0; i < len(filter); i++ {
		c := filter[i]
		if mustEscape(c) {
			buf[j+0] = '\\'
			buf[j+1] = hex[c>>4]
			buf[j+2] = hex[c&0xf]
			j += 3
		} else {
			buf[j] = c
			j++
		}
	}
	return string(buf)
}
