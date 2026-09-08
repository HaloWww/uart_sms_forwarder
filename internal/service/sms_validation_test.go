package service

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSMSContentUsesSafeByteLimit(t *testing.T) {
	if err := ValidateSMSContent(strings.Repeat("a", MaxSMSContentBytes)); err != nil {
		t.Fatalf("content at limit rejected: %v", err)
	}
	if err := ValidateSMSContent(strings.Repeat("中", MaxSMSContentBytes/3)); err != nil {
		t.Fatalf("UTF-8 content below byte limit rejected: %v", err)
	}
	if err := ValidateSMSContent(strings.Repeat("a", MaxSMSContentBytes+1)); !errors.Is(err, ErrSMSContentTooLong) {
		t.Fatalf("oversized content error=%v, want ErrSMSContentTooLong", err)
	}
}

func TestValidateSMSDestinationRejectsProtocolMarkerAndOversize(t *testing.T) {
	for _, destination := range []string{
		"",
		"123:CMD_END",
		strings.Repeat("1", MaxSMSDestinationBytes+1),
	} {
		if err := ValidateSMSDestination(destination); !errors.Is(err, ErrSMSDestinationInvalid) {
			t.Errorf("ValidateSMSDestination(%q) error = %v", destination, err)
		}
	}
	if err := ValidateSMSDestination("+8613800138000"); err != nil {
		t.Fatalf("valid international destination rejected: %v", err)
	}
}
