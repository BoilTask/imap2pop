package main

import (
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

type pop3Session struct {
	conn     net.Conn
	imap     *ImapConnection
	state    string // AUTH, TRANSACTION, CLOSED
	username string
	buf      string
	sessionID string

	msgCount  int
	totalSize int
}

func handlePop3Session(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	s := &pop3Session{
		conn:      conn,
		state:     "AUTH",
		sessionID: remote,
	}
	s.send("+OK POP3 IMAP proxy ready")

	defer func() {
		if s.imap != nil {
			s.imap.close()
		}
		conn.Close()
		log.Printf("[%s] POP3 session ended", s.sessionID)
	}()

	for {
		conn.SetDeadline(time.Time{})
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			if err != net.ErrClosed {
				log.Printf("[%s] POP3 read error: %v", s.sessionID, err)
			}
			return
		}
		s.buf += string(buf[:n])

		for {
			idx := strings.Index(s.buf, "\r\n")
			if idx == -1 {
				idx = strings.Index(s.buf, "\n")
				if idx == -1 {
					break
				}
			}
			line := strings.TrimSpace(s.buf[:idx])
			s.buf = s.buf[idx+2:]
			if line != "" {
				log.Printf("[%s] POP3 <<< %s", s.sessionID, line)
				s.dispatch(line)
				if s.state == "CLOSED" {
					return
				}
			}
		}
	}
}

func (s *pop3Session) send(line string) {
	log.Printf("[%s] POP3 >>> %s", s.sessionID, line)
	if s.conn != nil {
		s.conn.Write([]byte(line + "\r\n"))
	}
}

func (s *pop3Session) sendMulti(lines []string) {
	if s.conn == nil {
		return
	}
	var escaped []string
	for _, l := range lines {
		if strings.HasPrefix(l, ".") {
			escaped = append(escaped, "."+l)
		} else {
			escaped = append(escaped, l)
		}
	}
	data := strings.Join(escaped, "\r\n") + "\r\n.\r\n"
	log.Printf("[%s] POP3 >>> multi-line response (%d lines)", s.sessionID, len(escaped))
	s.conn.Write([]byte(data))
}

func (s *pop3Session) dispatch(line string) {
	parts := strings.SplitN(line, " ", 3)
	cmd := strings.ToUpper(parts[0])
	arg1 := ""
	arg2 := ""
	if len(parts) > 1 {
		arg1 = parts[1]
	}
	if len(parts) > 2 {
		arg2 = parts[2]
	}

	switch cmd {
	case "CAPA":
		s.cmdCapa()
	case "USER":
		s.cmdUser(arg1)
	case "PASS":
		s.cmdPass(arg1)
	case "STAT":
		s.cmdStat()
	case "LIST":
		s.cmdList(arg1)
	case "RETR":
		s.cmdRetr(arg1)
	case "DELE":
		s.cmdDele(arg1)
	case "UIDL":
		s.cmdUidl(arg1)
	case "TOP":
		s.cmdTop(arg1, arg2)
	case "NOOP":
		s.send("+OK")
	case "RSET":
		s.cmdRset()
	case "QUIT":
		s.cmdQuit()
	case "AUTH":
		s.send("-ERR AUTH not supported, use USER/PASS")
	default:
		s.send("-ERR Unknown command")
	}
}

func (s *pop3Session) cmdCapa() {
	s.sendMulti([]string{
		"+OK Capability list follows",
		"USER",
		"PASS",
		"STAT",
		"LIST",
		"RETR",
		"DELE",
		"UIDL",
		"TOP",
		"NOOP",
		"RSET",
		"QUIT",
	})
}

func (s *pop3Session) cmdUser(arg string) {
	if s.state != "AUTH" {
		s.send("-ERR USER only valid in AUTH state")
		return
	}
	s.username = arg
	s.send("+OK User accepted")
}

func (s *pop3Session) cmdPass(arg string) {
	if s.state != "AUTH" || s.username == "" {
		s.send("-ERR PASS requires USER first")
		return
	}

	ic := newImapConnection(s.sessionID)
	if err := ic.connect(); err != nil {
		s.send("-ERR Login failed: " + err.Error())
		return
	}
	if err := ic.login(s.username, arg); err != nil {
		ic.close()
		s.send("-ERR Login failed: " + err.Error())
		s.username = ""
		return
	}
	if err := ic.selectInbox(); err != nil {
		ic.close()
		s.send("-ERR Login failed: " + err.Error())
		s.username = ""
		return
	}
	if err := ic.fetchSizes(); err != nil {
		ic.close()
		s.send("-ERR Login failed: " + err.Error())
		s.username = ""
		return
	}

	s.imap = ic
	s.state = "TRANSACTION"
	s.msgCount = ic.messageCount
	s.totalSize = 0
	for _, sz := range ic.messageSizes {
		s.totalSize += sz
	}
	s.send("+OK Login successful")
}

func (s *pop3Session) cmdStat() {
	if s.state != "TRANSACTION" {
		s.send("-ERR STAT only valid in TRANSACTION state")
		return
	}
	s.send("+OK " + strconv.Itoa(s.msgCount) + " " + strconv.Itoa(s.totalSize))
}

func (s *pop3Session) cmdList(arg string) {
	if s.state != "TRANSACTION" {
		s.send("-ERR LIST only valid in TRANSACTION state")
		return
	}

	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > s.msgCount || s.imap.deleted[n] {
			s.send("-ERR No such message")
			return
		}
		sz := s.imap.messageSizes[n]
		s.send("+OK " + strconv.Itoa(n) + " " + strconv.Itoa(sz))
		return
	}

	var lines []string
	lines = append(lines, "+OK")
	for i := 1; i <= s.msgCount; i++ {
		if !s.imap.deleted[i] {
			lines = append(lines, strconv.Itoa(i)+" "+strconv.Itoa(s.imap.messageSizes[i]))
		}
	}
	s.sendMulti(lines)
}

func (s *pop3Session) cmdRetr(arg string) {
	if s.state != "TRANSACTION" {
		s.send("-ERR RETR only valid in TRANSACTION state")
		return
	}

	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > s.msgCount {
		s.send("-ERR Invalid message number")
		return
	}
	if s.imap.deleted[n] {
		s.send("-ERR Message already deleted")
		return
	}

	msg, err := s.imap.fetchMessage(n)
	if err != nil {
		s.send("-ERR RETR failed: " + err.Error())
		return
	}
	if msg == "" {
		s.send("-ERR Could not retrieve message")
		return
	}

	sz := s.imap.messageSizes[n]
	if sz == 0 {
		sz = len(msg)
	}
	msgLines := strings.Split(msg, "\n")
	var formatted []string
	formatted = append(formatted, "+OK "+strconv.Itoa(sz)+" octets")
	for _, l := range msgLines {
		l = strings.TrimRight(l, "\r")
		formatted = append(formatted, l)
	}
	s.sendMulti(formatted)
}

func (s *pop3Session) cmdDele(arg string) {
	if s.state != "TRANSACTION" {
		s.send("-ERR DELE only valid in TRANSACTION state")
		return
	}

	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > s.msgCount {
		s.send("-ERR Invalid message number")
		return
	}

	if err := s.imap.deleteMessage(n); err != nil {
		s.send("-ERR DELE failed: " + err.Error())
		return
	}
	s.send("+OK Message deleted")
}

func (s *pop3Session) cmdUidl(arg string) {
	if s.state != "TRANSACTION" {
		s.send("-ERR UIDL only valid in TRANSACTION state")
		return
	}

	if len(s.imap.messageUids) == 0 {
		if err := s.imap.fetchUids(); err != nil {
			s.send("-ERR UIDL failed: " + err.Error())
			return
		}
	}

	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > s.msgCount {
			s.send("-ERR No such message")
			return
		}
		uid := s.imap.messageUids[n]
		if uid == "" {
			uid = strconv.Itoa(n)
		}
		s.send("+OK " + strconv.Itoa(n) + " " + uid)
		return
	}

	var lines []string
	lines = append(lines, "+OK")
	for i := 1; i <= s.msgCount; i++ {
		if !s.imap.deleted[i] {
			uid := s.imap.messageUids[i]
			if uid == "" {
				uid = strconv.Itoa(i)
			}
			lines = append(lines, strconv.Itoa(i)+" "+uid)
		}
	}
	s.sendMulti(lines)
}

func (s *pop3Session) cmdTop(arg1, arg2 string) {
	if s.state != "TRANSACTION" {
		s.send("-ERR TOP only valid in TRANSACTION state")
		return
	}

	n, err := strconv.Atoi(arg1)
	lineCount := 0
	if arg2 != "" {
		lineCount, _ = strconv.Atoi(arg2)
	}

	if err != nil || n < 1 || n > s.msgCount {
		s.send("-ERR Invalid message number")
		return
	}
	if s.imap.deleted[n] {
		s.send("-ERR Message already deleted")
		return
	}

	content, err := s.imap.fetchTop(n, lineCount)
	if err != nil {
		s.send("-ERR TOP failed: " + err.Error())
		return
	}

	contentLines := strings.Split(content, "\n")
	var formatted []string
	formatted = append(formatted, "+OK")
	for _, l := range contentLines {
		l = strings.TrimRight(l, "\r")
		formatted = append(formatted, l)
	}
	s.sendMulti(formatted)
}

func (s *pop3Session) cmdRset() {
	if s.state != "TRANSACTION" {
		s.send("+OK")
		return
	}
	if err := s.imap.resetDeleted(); err != nil {
		s.send("-ERR Reset failed")
		return
	}
	s.send("+OK Reset")
}

func (s *pop3Session) cmdQuit() {
	if s.state == "TRANSACTION" && s.imap != nil {
		if len(s.imap.deleted) > 0 {
			_ = s.imap.expunge()
		}
		s.imap.logout()
	} else if s.imap != nil {
		s.imap.close()
	}
	s.send("+OK Goodbye")
	s.state = "CLOSED"
}