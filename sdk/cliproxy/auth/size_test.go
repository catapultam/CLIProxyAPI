package auth

import (
	"encoding/json"
	"math"
	"testing"
)

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

// TestApplyAuthSizeMetadataRejectsNaNAndInf covers strconv.ParseFloat's
// permissive parsing of "NaN", "inf", "-inf", and "Infinity" (case
// insensitive, no error) and the float64 forms of the same values, all of
// which parseSizeValue must still treat as invalid rather than as a "known"
// size.
func TestApplyAuthSizeMetadataRejectsNaNAndInf(t *testing.T) {
	for _, raw := range []any{
		"NaN", "nan", "inf", "-inf", "Infinity", "-Infinity",
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		auth := &Auth{}
		ApplyAuthSizeMetadata(auth, map[string]any{"size": raw})
		if _, ok := auth.Attributes[AttributeSize]; ok {
			t.Errorf("source %v set a size attribute", raw)
		}
		if _, ok := auth.Attributes[AttributeFileSize]; ok {
			t.Errorf("source %v set the file size marker", raw)
		}
		if auth.Metadata != nil {
			t.Errorf("source %v set metadata %v", raw, auth.Metadata)
		}
	}
}

func TestAuthSizeRejectsNaNAndInfAttribute(t *testing.T) {
	for _, raw := range []string{"NaN", "nan", "inf", "-inf", "Infinity", "-Infinity"} {
		a := &Auth{Attributes: map[string]string{AttributeSize: raw}}
		if _, ok := authSize(a); ok {
			t.Errorf("size %q reported known", raw)
		}
	}
}

// TestAuthSizeFallsBackToMetadataWhenAttributeAbsentOrEmpty covers every
// JSON-compatible numeric type a Metadata["size"] can hold: a
// management-panel PATCH writes it as json.Number, a file load as float64,
// and other callers may use int/int64/float32 or a numeric string.
func TestAuthSizeFallsBackToMetadataWhenAttributeAbsentOrEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
	}{
		{"float64", float64(20)},
		{"float32", float32(20)},
		{"int", int(20)},
		{"int64", int64(20)},
		{"json.Number", json.Number("20")},
		{"numeric string", "20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Metadata: map[string]any{AttributeSize: tc.raw}}
			got, ok := authSize(a)
			if !ok || got != 20 {
				t.Fatalf("authSize() = %v, %v; want 20, true", got, ok)
			}
			// An empty (but present) Attributes map is the same as absent.
			a.Attributes = map[string]string{}
			got, ok = authSize(a)
			if !ok || got != 20 {
				t.Fatalf("authSize() with empty attributes = %v, %v; want 20, true", got, ok)
			}
		})
	}
}

func TestAuthSizeMetadataFallbackRejectsNaNAndInfAndNonPositive(t *testing.T) {
	for _, raw := range []any{
		math.NaN(), math.Inf(1), math.Inf(-1), "NaN", "inf",
		float64(0), float64(-5), "not-a-number", true, nil,
	} {
		a := &Auth{Metadata: map[string]any{AttributeSize: raw}}
		if _, ok := authSize(a); ok {
			t.Errorf("metadata size %v reported known", raw)
		}
	}
}

// TestAuthSizeAttributeWinsOverMetadata covers both directions of
// precedence: a valid attribute is used even when metadata disagrees, and
// an invalid (but present) attribute reports unknown even when metadata
// holds a perfectly valid size -- the attribute, not the larger fallback,
// decides.
func TestAuthSizeAttributeWinsOverMetadata(t *testing.T) {
	a := &Auth{
		Attributes: map[string]string{AttributeSize: "5"},
		Metadata:   map[string]any{AttributeSize: float64(99)},
	}
	if got, ok := authSize(a); !ok || got != 5 {
		t.Fatalf("authSize() = %v, %v; want 5, true (attribute over metadata)", got, ok)
	}

	invalid := &Auth{
		Attributes: map[string]string{AttributeSize: "not-a-number"},
		Metadata:   map[string]any{AttributeSize: float64(99)},
	}
	if _, ok := authSize(invalid); ok {
		t.Fatal("an invalid attribute must not fall back to a valid metadata value")
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
