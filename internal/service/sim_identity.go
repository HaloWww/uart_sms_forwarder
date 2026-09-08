package service

import (
	"strconv"
	"strings"
)

const minimumSIMIdentityProtocolVersion = "1.3.0"

type SIMIdentity struct {
	SIMID    string
	ICCID    string
	IMSI     string
	IMEI     string
	MUID     string
	SIMSlot  int
	Revision uint64
	Verified bool
}

func normalizeIdentityValue(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "unknown", "null", "nil":
		return ""
	default:
		return value
	}
}

// makeSIMID 使用 ICCID 作为业务身份。ICCID 未确认时不使用易变化的串口或 IMEI 代替。
func makeSIMID(iccid string) string {
	iccid = normalizeIdentityValue(iccid)
	if iccid == "" {
		return ""
	}
	return "iccid:" + iccid
}

func identityFromStatus(status *StatusData) SIMIdentity {
	if status == nil {
		return SIMIdentity{}
	}
	return SIMIdentity{
		SIMID:    status.SIMID,
		ICCID:    normalizeIdentityValue(status.Mobile.Iccid),
		IMSI:     normalizeIdentityValue(status.Mobile.Imsi),
		IMEI:     normalizeIdentityValue(status.Mobile.Imei),
		MUID:     normalizeIdentityValue(status.MUID),
		SIMSlot:  status.SIMSlot,
		Revision: status.IdentityRevision,
		Verified: status.IdentityValid,
	}
}

func supportsSIMIdentityProtocol(version string) bool {
	actual := strings.Split(strings.TrimSpace(strings.TrimPrefix(version, "v")), ".")
	minimum := strings.Split(minimumSIMIdentityProtocolVersion, ".")
	for len(actual) < len(minimum) {
		actual = append(actual, "0")
	}
	for i := range minimum {
		actualPart, err := strconv.Atoi(actual[i])
		if err != nil {
			return false
		}
		minimumPart, _ := strconv.Atoi(minimum[i])
		if actualPart != minimumPart {
			return actualPart > minimumPart
		}
	}
	return true
}
