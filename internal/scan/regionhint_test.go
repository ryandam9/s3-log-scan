package scan

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestRegionHint(t *testing.T) {
	for _, code := range []string{"IllegalLocationConstraintException", "PermanentRedirect", "AuthorizationHeaderMalformed"} {
		err := &smithy.GenericAPIError{Code: code}
		if hint := regionHint(err); !strings.Contains(hint, "-region") {
			t.Errorf("%s: no hint produced", code)
		}
	}
	if hint := regionHint(&smithy.GenericAPIError{Code: "AccessDenied"}); hint != "" {
		t.Errorf("unrelated error must not get a region hint: %q", hint)
	}
	if hint := regionHint(errors.New("plain")); hint != "" {
		t.Errorf("non-API error must not get a region hint: %q", hint)
	}
}

// The hint reaches the listing error the operator actually sees.
func TestListingErrorCarriesRegionHint(t *testing.T) {
	f := newFakeS3(2)
	for i := 0; i < 5; i++ {
		f.put("logs/x.log", "x\n")
	}
	f.put("logs/y.log", "y\n")
	f.put("logs/z.log", "z\n")
	f.listErr = &smithy.GenericAPIError{Code: "IllegalLocationConstraintException", Message: "location constraint incompatible"}
	res, _, _ := runEngine(t, testConfig(t, "ERROR"), f)
	if res.ListingErr == nil {
		t.Fatal("listing error expected")
	}
	if !strings.Contains(res.ListingErr.Error(), "rerun with -region") {
		t.Fatalf("hint missing: %v", res.ListingErr)
	}
}
