package modem

//Listener ...
type Listener interface {
	ReceiveSms(modem *Modem, id int)
}
