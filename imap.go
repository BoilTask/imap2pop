package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
)

const imapHost = "imap.feishu.cn"
const imapPort = 993

type imapResult struct {
	lines    []string
	literals map[int]string
	tagged   string
}

type ImapConnection struct {
	conn       net.Conn
	reader     *bufio.Reader
	tagCounter int
	sessionID  string

	messageSizes map[int]int
	messageUids  map[int]string
	deleted      map[int]bool
	messageCount int
}

func newImapConnection(sessionID string) *ImapConnection {
	return &ImapConnection{
		sessionID:    sessionID,
		messageSizes: make(map[int]int),
		messageUids:  make(map[int]string),
		deleted:      make(map[int]bool),
	}
}

func (ic *ImapConnection) connect() error {
	cfg := &tls.Config{InsecureSkipVerify: true}
	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", imapHost, imapPort), cfg)
	if err != nil {
		log.Printf("[%s] IMAP connect failed: %v", ic.sessionID, err)
		return fmt.Errorf("IMAP connect failed: %v", err)
	}
	ic.conn = conn
	ic.reader = bufio.NewReader(conn)

	greeting, err := ic.reader.ReadString('\n')
	if err != nil {
		conn.Close()
		log.Printf("[%s] IMAP greeting read failed: %v", ic.sessionID, err)
		return fmt.Errorf("IMAP greeting read failed: %v", err)
	}
	greeting = strings.TrimRight(greeting, "\r\n")
	up := strings.ToUpper(greeting)
	if !strings.HasPrefix(up, "* OK") && !strings.HasPrefix(up, "* PREAUTH") {
		conn.Close()
		log.Printf("[%s] Bad IMAP greeting: %s", ic.sessionID, greeting)
		return fmt.Errorf("Bad IMAP greeting: %s", greeting)
	}
	log.Printf("[%s] IMAP connected, greeting: %s", ic.sessionID, greeting)
	return nil
}

func (ic *ImapConnection) nextTag() string {
	ic.tagCounter++
	return fmt.Sprintf("A%d", ic.tagCounter)
}

func (ic *ImapConnection) sendCommand(cmd string) (imapResult, error) {
	tag := ic.nextTag()
	fullCmd := fmt.Sprintf("%s %s", tag, cmd)
	log.Printf("[%s] IMAP >>> %s", ic.sessionID, fullCmd)

	if _, err := fmt.Fprintf(ic.conn, "%s\r\n", fullCmd); err != nil {
		log.Printf("[%s] IMAP send failed: %v", ic.sessionID, err)
		return imapResult{}, fmt.Errorf("send IMAP command failed: %v", err)
	}
	return ic.readResponse(tag)
}

func (ic *ImapConnection) readResponse(tag string) (imapResult, error) {
	var lines []string
	literals := make(map[int]string)

	for {
		line, literal, err := ic.readLine()
		if err != nil {
			log.Printf("[%s] IMAP read error: %v", ic.sessionID, err)
			return imapResult{}, fmt.Errorf("read IMAP response: %v", err)
		}

		if strings.HasPrefix(line, tag+" ") {
			log.Printf("[%s] IMAP <<< %s", ic.sessionID, line)
			return imapResult{lines: lines, literals: literals, tagged: line}, nil
		}

		log.Printf("[%s] IMAP <<< %s", ic.sessionID, line)
		idx := len(lines)
		lines = append(lines, line)
		if literal != "" {
			literals[idx] = literal
			log.Printf("[%s] IMAP <<< [literal %d bytes]", ic.sessionID, len(literal))
		}
	}
}

func (ic *ImapConnection) readLine() (string, string, error) {
	line, err := ic.reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	line = strings.TrimRight(line, "\r\n")

	// Check for IMAP literal: {N}
	litRe := regexp.MustCompile(`\{(\d+)\}$`)
	m := litRe.FindStringSubmatch(line)
	if m == nil {
		return line, "", nil
	}

	litLen, _ := strconv.Atoi(m[1])

	litBuf := make([]byte, litLen)
	if _, err := io.ReadFull(ic.reader, litBuf); err != nil {
		return "", "", fmt.Errorf("read literal failed: %v", err)
	}
	literal := string(litBuf)

	afterLine, err := ic.reader.ReadString('\n')
	if err != nil {
		return "", "", fmt.Errorf("read after literal failed: %v", err)
	}
	afterLine = strings.TrimRight(afterLine, "\r\n")

	line = litRe.ReplaceAllString(line, "") + afterLine
	return line, literal, nil
}

func (ic *ImapConnection) login(username, password string) error {
	result, err := ic.sendCommand(fmt.Sprintf(`LOGIN "%s" "%s"`, imapEscape(username), imapEscape(password)))
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		log.Printf("[%s] IMAP LOGIN failed: %s", ic.sessionID, result.tagged)
		return fmt.Errorf("IMAP LOGIN failed: %s", result.tagged)
	}
	log.Printf("[%s] IMAP LOGIN OK for %s", ic.sessionID, username)
	return nil
}

func (ic *ImapConnection) selectInbox() error {
	result, err := ic.sendCommand("SELECT INBOX")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		log.Printf("[%s] IMAP SELECT failed: %s", ic.sessionID, result.tagged)
		return fmt.Errorf("IMAP SELECT failed: %s", result.tagged)
	}

	ic.messageSizes = make(map[int]int)
	ic.messageUids = make(map[int]string)
	ic.deleted = make(map[int]bool)
	ic.messageCount = 0

	existsRe := regexp.MustCompile(`\* (\d+) EXISTS`)
	for _, line := range result.lines {
		m := existsRe.FindStringSubmatch(line)
		if m != nil {
			ic.messageCount, _ = strconv.Atoi(m[1])
		}
	}
	log.Printf("[%s] IMAP SELECT OK, %d messages in INBOX", ic.sessionID, ic.messageCount)
	return nil
}

func (ic *ImapConnection) fetchSizes() error {
	result, err := ic.sendCommand("FETCH 1:* (RFC822.SIZE)")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("FETCH sizes failed: %s", result.tagged)
	}

	sizeRe := regexp.MustCompile(`\* (\d+) FETCH \(RFC822\.SIZE (\d+)\)`)
	for _, line := range result.lines {
		m := sizeRe.FindStringSubmatch(line)
		if m != nil {
			seq, _ := strconv.Atoi(m[1])
			size, _ := strconv.Atoi(m[2])
			ic.messageSizes[seq] = size
		}
	}
	log.Printf("[%s] FETCH sizes OK, %d entries", ic.sessionID, len(ic.messageSizes))
	return nil
}

func (ic *ImapConnection) fetchUids() error {
	result, err := ic.sendCommand("FETCH 1:* (UID)")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("FETCH UIDs failed: %s", result.tagged)
	}

	uidRe := regexp.MustCompile(`\* (\d+) FETCH \(UID (\d+)\)`)
	for _, line := range result.lines {
		m := uidRe.FindStringSubmatch(line)
		if m != nil {
			seq, _ := strconv.Atoi(m[1])
			ic.messageUids[seq] = m[2]
		}
	}
	log.Printf("[%s] FETCH UIDs OK, %d entries", ic.sessionID, len(ic.messageUids))
	return nil
}

func (ic *ImapConnection) fetchMessage(seqNum int) (string, error) {
	result, err := ic.sendCommand(fmt.Sprintf("FETCH %d (RFC822)", seqNum))
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return "", fmt.Errorf("FETCH message failed: %s", result.tagged)
	}
	msg := ic.getLiteralForSeq(result, seqNum)
	log.Printf("[%s] FETCH message #%d, %d bytes", ic.sessionID, seqNum, len(msg))
	return msg, nil
}

func (ic *ImapConnection) fetchHeaders(seqNum int) (string, error) {
	result, err := ic.sendCommand(fmt.Sprintf("FETCH %d (BODY[HEADER])", seqNum))
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return "", fmt.Errorf("FETCH headers failed: %s", result.tagged)
	}
	h := ic.getLiteralForSeq(result, seqNum)
	log.Printf("[%s] FETCH headers #%d, %d bytes", ic.sessionID, seqNum, len(h))
	return h, nil
}

func (ic *ImapConnection) fetchTop(seqNum int, lineCount int) (string, error) {
	headers, err := ic.fetchHeaders(seqNum)
	if err != nil {
		return "", err
	}
	if lineCount == 0 {
		return headers, nil
	}

	bodyResult, err := ic.sendCommand(fmt.Sprintf("FETCH %d (BODY[TEXT])", seqNum))
	if err != nil {
		return headers, nil
	}
	if !strings.Contains(strings.ToUpper(bodyResult.tagged), "OK") {
		return headers, nil
	}
	body := ic.getLiteralForSeq(bodyResult, seqNum)

	bodyLines := strings.Split(body, "\n")
	var filtered []string
	for _, l := range bodyLines {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			filtered = append(filtered, l)
		}
		if len(filtered) >= lineCount {
			break
		}
	}
	if len(filtered) == 0 {
		return headers, nil
	}
	log.Printf("[%s] FETCH TOP #%d, %d body lines", ic.sessionID, seqNum, len(filtered))
	return headers + "\r\n" + strings.Join(filtered, "\r\n"), nil
}

func (ic *ImapConnection) getLiteralForSeq(result imapResult, seqNum int) string {
	fetchRe := regexp.MustCompile(fmt.Sprintf(`\* %d FETCH`, seqNum))
	for i, line := range result.lines {
		if fetchRe.MatchString(line) {
			if lit, ok := result.literals[i]; ok {
				return lit
			}
		}
	}
	return ""
}

func (ic *ImapConnection) deleteMessage(seqNum int) error {
	result, err := ic.sendCommand(fmt.Sprintf("STORE %d +FLAGS (\\Deleted)", seqNum))
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("STORE +FLAGS failed: %s", result.tagged)
	}
	ic.deleted[seqNum] = true
	log.Printf("[%s] DELE #%d OK", ic.sessionID, seqNum)
	return nil
}

func (ic *ImapConnection) resetDeleted() error {
	if len(ic.deleted) == 0 {
		return nil
	}
	for seqNum := range ic.deleted {
		_, _ = ic.sendCommand(fmt.Sprintf("STORE %d -FLAGS (\\Deleted)", seqNum))
	}
	ic.deleted = make(map[int]bool)
	log.Printf("[%s] RSET OK, cleared %d deletions", ic.sessionID, len(ic.deleted))
	return nil
}

func (ic *ImapConnection) expunge() error {
	_, err := ic.sendCommand("EXPUNGE")
	if err != nil {
		return err
	}
	log.Printf("[%s] EXPUNGE OK", ic.sessionID)
	return nil
}

func (ic *ImapConnection) logout() {
	_, _ = ic.sendCommand("LOGOUT")
	ic.close()
	log.Printf("[%s] IMAP session closed", ic.sessionID)
}

func (ic *ImapConnection) close() {
	if ic.conn != nil {
		ic.conn.Close()
		ic.conn = nil
	}
}

func imapEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}