package auth

import "testing"

func TestApplyAuthSizeMetadataClearsUntrustedFileMarker(t *testing.T) {
	for _, metadata := range []map[string]any{
		{},
		{"size": "bad"},
		{"size": 0},
		{"size": -5},
	} {
		auth := &Auth{
			Attributes: map[string]string{"size": "7", AttributeFileSize: "true"},
			Metadata:   map[string]any{"size": float64(7)},
		}
		ApplyAuthSizeMetadata(auth, metadata)
		if _, inherited := auth.Attributes[AttributeFileSize]; inherited {
			t.Errorf("source %v kept untrusted file size marker", metadata)
		}
		if auth.Attributes["size"] != "7" || auth.Metadata["size"] != float64(7) {
			t.Errorf("source %v changed existing size %q/%v", metadata, auth.Attributes["size"], auth.Metadata["size"])
		}
	}
}

func TestApplyAuthSizeMetadataParsesIntFloatAndNumericString(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want string
	}{
		{"int-valued float64 (JSON int)", float64(5), "5"},
		{"float64", float64(2.5), "2.5"},
		{"numeric string", "20", "20"},
		{"numeric string with whitespace", " 12.5 ", "12.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{}
			ApplyAuthSizeMetadata(auth, map[string]any{"size": tc.raw})
			if got := auth.Attributes[AttributeSize]; got != tc.want {
				t.Errorf("size attribute = %q, want %q", got, tc.want)
			}
			if got := auth.Attributes[AttributeFileSize]; got != "true" {
				t.Errorf("file size marker = %q, want true", got)
			}
			if got := auth.Metadata["size"]; got != tc.raw {
				t.Errorf("size metadata = %v, want %v", got, tc.raw)
			}
		})
	}
}

func TestApplyAuthSizeMetadataIgnoresMissingZeroNegativeAndGarbage(t *testing.T) {
	for _, metadata := range []map[string]any{
		{},
		{"size": 0},
		{"size": float64(0)},
		{"size": -1},
		{"size": "-3"},
		{"size": "garbage"},
		{"size": true},
		{"size": nil},
	} {
		auth := &Auth{}
		ApplyAuthSizeMetadata(auth, metadata)
		if _, ok := auth.Attributes[AttributeSize]; ok {
			t.Errorf("source %v set a size attribute", metadata)
		}
		if _, ok := auth.Attributes[AttributeFileSize]; ok {
			t.Errorf("source %v set the file size marker", metadata)
		}
	}
}

func TestAuthSizeUnknownForMissingNonNumericOrNonPositive(t *testing.T) {
	if _, ok := authSize(nil); ok {
		t.Fatal("nil auth reported a known size")
	}
	if _, ok := authSize(&Auth{}); ok {
		t.Fatal("auth without attributes reported a known size")
	}
	for _, raw := range []string{"", "0", "-5", "not-a-number"} {
		a := &Auth{Attributes: map[string]string{AttributeSize: raw}}
		if _, ok := authSize(a); ok {
			t.Errorf("size %q reported known", raw)
		}
	}
}

func TestAuthSizeKnownForPositiveNumericAttribute(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"5", 5},
		{"20", 20},
		{"2.5", 2.5},
	} {
		a := &Auth{Attributes: map[string]string{AttributeSize: tc.raw}}
		got, ok := authSize(a)
		if !ok || got != tc.want {
			t.Errorf("authSize(%q) = %v, %v; want %v, true", tc.raw, got, ok, tc.want)
		}
	}
}
