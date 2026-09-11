package service

import (
	"io"
	"time"

	"github.com/valyala/fasttemplate"
)

func formatSMSForwardingMessage(template string, msg NotificationMessage) string {
	t := fasttemplate.New(template, "{{", "}}")
	return t.ExecuteFuncString(func(w io.Writer, tag string) (int, error) {
		value := ""
		switch tag {
		case "content":
			value = msg.Content
		case "from":
			value = msg.From
		case "receiver", "to":
			value = msg.To
		case "timestamp":
			value = time.Unix(msg.Timestamp, 0).Format(time.DateTime)
		case "device_id":
			value = msg.DeviceID
		case "device_name":
			value = msg.DeviceName
		case "sim_id":
			value = msg.SIMID
		case "iccid":
			value = msg.ICCID
		case "imsi":
			value = msg.IMSI
		case "imei":
			value = msg.IMEI
		default:
			return w.Write([]byte("{{" + tag + "}}"))
		}
		return w.Write([]byte(value))
	})
}
