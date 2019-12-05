package modem

type commandItem struct {
	command      int
	contents     []string
	responseCode int
	ch           chan interface{}
	finishCh     chan bool
	// step         int
}

func newCommandItem(command int, contents ...string) *commandItem {
	return &commandItem{command: command, contents: contents, ch: make(chan interface{}, 1), finishCh: make(chan bool, 1)}
}

func (c *commandItem) GetContents() []string {
	contents := c.contents
	switch c.command {
	case commandStatus:
		contents = []string{`AT` + newline}
	case commandCloseUSSD:
		contents = []string{`AT+CUSD=2` + newline}
	case commandUSSD:
		// newCommandItem(`AT+CUSD=1,"` + ussd + `",15`)
		contents = []string{`AT+CUSD=1,"` + c.contents[0] + `",15` + newline}
	case commandIMSI:
		contents = []string{`AT+CIMI` + newline}
	case commandDeleteSms:
		contents = []string{`AT+CMGD=` + c.contents[0] + newline}
	case commandDeleteAllSms:
		contents = []string{`AT+CMGD=1,4` + newline}
	case commandSendSms:
		contents = []string{
			`AT+CMGS="` + c.contents[0] + `"` + newline,
			`WAIT`,
			c.contents[1] + string(byte(26))}
	case commandListSms:
		contents = []string{`AT+CMGL="ALL"` + newline}
	case commandReadSms:
		contents = []string{`AT+CMGR=` + c.contents[0] + newline}

	}
	return contents
}
