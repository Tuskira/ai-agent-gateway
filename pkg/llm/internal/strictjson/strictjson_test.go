package strictjson

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1,"b":{"a":2},"c":[{"a":1},{"a":2}]}`: "",
		`{"a":1,"a":2}`:                         `duplicate key "a" in top-level object`,
		`{"m":[{"x":1},{"y":1,"y":2}]}`:         `duplicate key "y" in m.1`,
		`{"m":{"Role":"user","role":"system"}}`: `keys "Role" and "role" in m differ only by case`,
		`{"ſ":1,"S":2}`:                         `differ only by case`, // long s folds to s, as in encoding/json
		`{"a":1,"a":`:                           `duplicate key "a"`,   // checked up to the syntax error
		`{"a":1,`:                               "",                    // syntax is the parser's to judge
		`[DONE]`:                                "",
		strings.Repeat("[", 20000) + strings.Repeat("]", 20000): "",
	} {
		err := Check([]byte(in))
		switch {
		case want == "" && err != nil:
			t.Errorf("Check(%.40s) = %v, want nil", in, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("Check(%.40s) = %v, want %q", in, err, want)
		}
	}
}
