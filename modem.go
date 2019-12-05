package modem

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitlab.com/tuxer/go-logger"

	"go.bug.st/serial.v1"
)

var (
	newline     = "\r\n"
	end         = newline + `(OK|ERROR)` + newline
	strRegexSms = `"(.*)","(.*)",.*,"(.*)"` + newline + `(.*)` + newline

	regexStatus    = regexp.MustCompile(`(?s)^(OK|ERROR)` + newline)
	regexImsi      = regexp.MustCompile(`(?s)` + newline + `([0-9]+)` + newline)
	regexSmsNotif  = regexp.MustCompile(`\+CMTI: .*,([0-9]+)`)
	regexSmsRead   = regexp.MustCompile(`(?s)\+CMGR: ` + strRegexSms)
	regexListSms   = regexp.MustCompile(`(?s)\s*([0-9]+),` + strRegexSms)
	regexSendSms   = regexp.MustCompile(`(?s)\+CMGS: ([0-9]+)` + newline)
	regexDeleteSms = regexp.MustCompile(``)

	regexUssd = regexp.MustCompile(`(?s)\+CUSD: ([0-9]+)(,"(.*)",15|)` + newline)
)

//Sms ...
type Sms struct {
	ID      int
	Sender  string
	Time    string
	Content string
}

const (
	commandStd = 0 + iota
	commandStatus
	commandIMSI
	commandUSSD
	commandCloseUSSD
	commandListSms
	commandSendSms
	commandReadSms
	commandDeleteSms
	commandDeleteAllSms
	commandSMSNotification
	commandTextMode
)

type modemItem struct {
	cmd int
	ch  chan interface{}
}

//Modem ...
type Modem struct {
	Port           string
	imsi           string
	BaudRate       int
	Listener       Listener
	ConnectTimeout time.Duration

	serialPort  serial.Port
	writeCh     chan *commandItem
	currentItem *commandItem
	lineCh      chan []byte
	LogRequest  bool
}

//Connect ...
func (m *Modem) Connect() error {
	m.reset()
	mode := &serial.Mode{
		BaudRate: m.GetBaudRate(),
		DataBits: 8,
	}
	s, e := serial.Open(m.Port, mode)
	if e != nil {
		m.reset()
		return e
	}
	m.serialPort = s

	go m.processor()
	go m.reader()
	go m.writer()

	statusCh := make(chan bool, 1)

	go func() {
		statusCh <- m.GetStatus()
	}()
	timeout := m.ConnectTimeout
	if timeout == 0 {
		timeout = 5000 * time.Millisecond
	}
	select {
	case status := <-statusCh:
		if !status {
			m.reset()
			return errors.New(`ERROR`)
		}
	case <-time.After(timeout):
		return errors.New(`Timeout`)
	}

	m.GetImsi()
	return nil
}

//Imsi ...
func (m *Modem) Imsi() string {
	return m.imsi
}

//SetImsi ...
func (m *Modem) SetImsi(imsi string) {
	m.imsi = imsi
}

func (m *Modem) writer() {
	for m.serialPort != nil {
		log.D(`reading item`)
		m.currentItem = <-m.writeCh
		log.D(`receive item`)
		contents := m.currentItem.GetContents()
		for _, c := range contents {
			if c == `WAIT` {
				time.Sleep(100 * time.Millisecond)
			} else {
				if m.LogRequest {
					m.writeLog([]byte(c))
				}
				m.serialPort.Write([]byte(c))
			}
		}
		log.D(`waiting finish`)
		<-m.currentItem.finishCh
		log.D(`finish`)
	}
}

func (m *Modem) reader() {
	var buffer []byte
	for m.serialPort != nil {
		buff := make([]byte, 128)
		n, e := m.serialPort.Read(buff)
		if e == nil {
			if n > 0 {
				m.writeLog(buff[:n])
				buffer = append(buffer, buff[:n]...)
				for {
					idx := bytes.Index(buffer, []byte(newline))
					if idx < 0 {
						break
					}
					m.lineCh <- buffer[:idx+2]
					buffer = buffer[idx+2:]
				}
			}
		} else {
			m.serialPort = nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Modem) processor() {
	var buffer []byte
	for m.serialPort != nil {
		line := <-m.lineCh
		if bytes.HasPrefix(line, []byte(`+CMTI`)) {
			if m.Listener != nil {
				if idx := regexSmsNotif.FindSubmatchIndex(line); len(idx) > 0 {
					id, _ := strconv.Atoi(string(line[idx[2]:idx[3]]))
					go func() {
						time.Sleep(100 * time.Millisecond)
						m.Listener.ReceiveSms(m, id)
					}()
				}
			}
		} else if bytes.HasPrefix(line, []byte(`RING`)) { //TODO incoming call

		} else {
			item := m.currentItem
			if item == nil {
				item = &commandItem{}
			}
			complete := false

			switch item.command {
			case commandStatus:
				fallthrough
			case commandSMSNotification:
				fallthrough
			case commandTextMode:
				fallthrough
			case commandDeleteSms:
				if status := parseStatus(line); status != nil {
					item.ch <- *status
					complete = true
				}
			case commandDeleteAllSms:
				if status := parseStatus(line); status != nil {
					item.ch <- *status
					complete = true
				}
			case commandIMSI:
				if status := parseStatus(line); status != nil {
					if *status == `OK` {
						idx := regexImsi.FindSubmatchIndex(buffer)
						m.imsi = string(buffer[idx[2]:idx[3]])
					}
					item.ch <- m.imsi
					complete = true
				}
			case commandReadSms:
				if status := parseStatus(line); status != nil {
					if *status == `OK` {
						idx := regexSmsRead.FindSubmatchIndex(buffer)

						sms := &Sms{
							Sender:  string(buffer[idx[4]:idx[5]]),
							Time:    string(buffer[idx[6]:idx[7]]),
							Content: string(buffer[idx[8]:idx[9]]),
						}
						item.ch <- sms
					} else {
						item.ch <- nil
					}
					complete = true
				}
			case commandListSms:
				if status := parseStatus(line); status != nil {
					if *status == `OK` {
						smses := bytes.Split(buffer, []byte(`+CMGL: `))
						listSms := []*Sms{}
						for _, val := range smses {
							matches := regexListSms.FindStringSubmatch(string(val))
							log.D(`SMS:`, string(val))
							if len(matches) > 5 {
								log.D(`1`, matches[1])
								log.D(`2`, matches[2])
								if len(matches) > 0 {
									sms := &Sms{
										Sender:  matches[3],
										Time:    matches[4],
										Content: matches[5],
									}
									sms.ID, _ = strconv.Atoi(matches[1])
									listSms = append(listSms, sms)
								}
							} else {
								log.D(`Len SMS:`, len(matches))
							}
						}
						item.ch <- listSms
					} else {
						item.ch <- nil
					}
					complete = true
				}
			case commandSendSms:
				if status := parseStatus(line); status != nil {
					if *status == `OK` {
						matches := regexSendSms.FindSubmatch(buffer)
						id, _ := strconv.Atoi(string(matches[1]))
						item.ch <- id
					} else {
						item.ch <- *status
					}
					complete = true
				}
			case commandUSSD:
				if matches := regexUssd.FindSubmatch(append(buffer, line...)); len(matches) > 0 {
					if len(matches[2]) > 0 {
						item.ch <- matches[3]
					} else {
						intStatus, _ := strconv.Atoi(string(matches[1]))
						item.ch <- intStatus
					}
					complete = true
				}
			}
			if complete {
				item.finishCh <- true
				buffer = []byte{}
			} else {
				buffer = append(buffer, line...)
			}
		}
	}
}

func parseStatus(line []byte) *string {
	if idx := regexStatus.FindSubmatchIndex(line); len(idx) > 0 {
		status := string(line[idx[2]:idx[3]])
		return &status
	}
	return nil
}

//GetStatus ...
func (m *Modem) GetStatus() bool {
	return m.sendCommand(commandStatus)
}

//GetImsi ...
func (m *Modem) GetImsi() string {
	b := m.send(commandIMSI)
	m.imsi = b.(string)
	return m.imsi
}

//GetSms ...
func (m *Modem) GetSms(id int) *Sms {
	item := newCommandItem(commandReadSms, strconv.Itoa(id))
	m.writeCh <- item
	select {
	case resp := <-item.ch:
		if resp != nil {
			sms := resp.(*Sms)
			sms.ID = id
			return sms
		}
	case <-time.After(10 * time.Second):
	}
	return nil
}

//ListSms ...
func (m *Modem) ListSms() ([]*Sms, error) {
	item := newCommandItem(commandListSms)
	m.writeCh <- item
	select {
	case resp := <-item.ch:
		if resp != nil {
			smses := resp.([]*Sms)
			return smses, nil
		}
		return nil, nil
	case <-time.After(10 * time.Second):
	}
	return nil, errors.New(`unable to get sms list`)
}

//SendSms ...
func (m *Modem) SendSms(destination, sms string) (int, error) {
	if destination[0:1] != `+` {
		destination = `+` + destination
	}
	item := newCommandItem(commandSendSms, destination, sms)
	m.writeCh <- item
	select {
	case resp := <-item.ch:
		switch resp := resp.(type) {
		case string:
			return 0, errors.New(resp)
		case int:
			return resp, nil
		}
	case <-time.After(10 * time.Second):
		//Timeout
	}
	return 0, errors.New(`Timeout`)
}

//DeleteSms ...
func (m *Modem) DeleteSms(ID int) bool {
	return m.sendCommand(commandDeleteSms, strconv.Itoa(ID))
}

//DeleteAllSms ...
func (m *Modem) DeleteAllSms() bool {
	return m.sendCommand(commandDeleteAllSms)
}

//ClearSms ...
func (m *Modem) ClearSms() bool {
	item := newCommandItem(commandDeleteSms, `1,4`)
	m.writeCh <- item
	return <-item.ch == `OK`
}

// SendUssd ...
func (m *Modem) SendUssd(ussd string) ([]byte, error) {
	item := newCommandItem(commandUSSD, ussd)
	m.writeCh <- item
	select {
	case resp := <-item.ch:
		switch resp := resp.(type) {
		case []byte:
			return resp, nil
		case int:
			return nil, errors.New(`Error ` + strconv.Itoa(resp))
		}
	case <-time.After(60 * time.Second):
	}
	return nil, errors.New(`Timeout`)
}

//EnableSmsNotification ...
func (m *Modem) EnableSmsNotification() bool {
	return m.sendCommand(commandSMSNotification, `AT+CNMI=1,1,2,1,0`+newline)
}

//SetTextMode ...
func (m *Modem) SetTextMode(mode bool) bool {
	cmd := `1`
	if mode == false {
		cmd = `0`
	}
	return m.sendCommand(commandTextMode, `AT+CMGF=`+cmd+newline)
}

func (m *Modem) send(cmd int) interface{} {
	item := newCommandItem(cmd)
	m.writeCh <- item
	return <-item.ch
}

func (m *Modem) sendCommand(cmd int, contents ...string) bool {
	item := newCommandItem(cmd, contents...)
	m.writeCh <- item
	return <-item.ch == `OK`
}

func (m *Modem) reset() {
	m.serialPort = nil
	m.writeCh = make(chan *commandItem)
	m.lineCh = make(chan []byte)
}

func (m *Modem) writeLog(data []byte) {
	os.MkdirAll(`log/modem`, 0775)
	paths := strings.Split(m.Port, `/`)
	f, e := os.OpenFile(`log/modem/`+paths[len(paths)-1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		log.E(e)
		return
	}
	defer f.Close()
	f.Write(data)
}

//Close ...
func (m *Modem) Close() error {
	if m.serialPort != nil {
		return m.serialPort.Close()
	}
	m.reset()
	return nil
}

//GetBaudRate ...
func (m *Modem) GetBaudRate() int {
	if m.BaudRate == 0 {
		m.BaudRate = 115200
	}
	return m.BaudRate
}

//New ...
func New(port string) *Modem {
	m := Modem{Port: port, BaudRate: 9600}
	return &m
}

//GetAllPorts ...
func GetAllPorts() ([]string, error) {
	return serial.GetPortsList()
}

//GetActiveModems ...
func GetActiveModems(baud int) []*Modem {
	ports, _ := GetAllPorts()
	modemCh := make(chan *Modem, len(ports))
	modems := []*Modem{}

	for _, val := range ports {
		if strings.HasPrefix(val, `/dev/tty`) {
			go func(port string) {
				m := &Modem{Port: port, BaudRate: baud}
				if e := m.Connect(); e == nil {
					modemCh <- m
				} else {
					modemCh <- nil
				}
			}(val)
		} else {
			modemCh <- nil
		}
	}
	for i := 0; i < len(ports); i++ {
		modem := <-modemCh
		if modem != nil {
			modems = append(modems, modem)
		}
	}
	return modems
}
