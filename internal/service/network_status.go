package service

import "fmt"

// The SIM's IMSI identifies its home network, not the network currently serving
// a roaming SIM. Only use the modem's current serving-cell MCC/MNC here.
func normalizeServingNetwork(status *StatusData) {
	mobile := &status.Mobile
	mobile.ServingOperator = ""
	if !mobile.ServingNetworkSupported || !mobile.IsRegistered ||
		verifiedStatusICCID(status) == "" || status.Flymode || mobile.Flymode ||
		mobile.ServingMCC == nil || mobile.ServingMNC == nil ||
		*mobile.ServingMCC < 1 || *mobile.ServingMCC > 999 ||
		*mobile.ServingMNC < 0 || *mobile.ServingMNC > 999 {
		mobile.ServingMCC, mobile.ServingMNC = nil, nil
		return
	}

	// scell returns numbers and does not preserve the MNC's original width.
	// Resolve only known table entries; do not expose a guessed PLMN string.
	mcc, mnc := *mobile.ServingMCC, *mobile.ServingMNC
	twoDigit := OperData[fmt.Sprintf("%03d%02d", mcc, mnc)]
	threeDigit := OperData[fmt.Sprintf("%03d%03d", mcc, mnc)]
	if twoDigit != "" && threeDigit != "" && twoDigit != threeDigit {
		return
	}
	if twoDigit != "" {
		mobile.ServingOperator = twoDigit
	} else {
		mobile.ServingOperator = threeDigit
	}
}
