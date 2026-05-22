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

var (
	reLiteral    = regexp.MustCompile(`\{(\d+)\}$`)
	reExists     = regexp.MustCompile(`\* (\d+) EXISTS`)
	reFetchSeq   = regexp.MustCompile(`\* (\d+) FETCH`)
	reSizeField  = regexp.MustCompile(`RFC822\.SIZE (\d+)`)
	reUidField   = regexp.MustCompile(`UID (\d+)`)
)

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
	messageHdrs  map[int]string // cached headers
	messageCache map[int]string // cached full messages (RETR)
	deleted      map[int]bool
	messageCount int
}

func newImapConnection(sessionID string) *ImapConnection {
	return &ImapConnection{
		sessionID:    sessionID,
		messageSizes: make(map[int]int),
		messageUids:  make(map[int]string),
		messageHdrs:  make(map[int]string),
		messageCache: make(map[int]string),
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

	// Set TCP_NODELAY on IMAP connection for lower latency
	if nc, ok := conn.NetConn().(*net.TCPConn); ok {
		nc.SetNoDelay(true)
	}

	greeting, err := ic.reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return fmt.Errorf("IMAP greeting read failed: %v", err)
	}
	greeting = strings.TrimRight(greeting, "\r\n")
	up := strings.ToUpper(greeting)
	if !strings.HasPrefix(up, "* OK") && !strings.HasPrefix(up, "* PREAUTH") {
		conn.Close()
		return fmt.Errorf("Bad IMAP greeting: %s", greeting)
	}
	log.Printf("[%s] IMAP connected", ic.sessionID)
	return nil
}

func (ic *ImapConnection) nextTag() string {
	ic.tagCounter++
	return fmt.Sprintf("A%d", ic.tagCounter)
}

func (ic *ImapConnection) sendCommand(cmd string) (imapResult, error) {
	tag := ic.nextTag()
	if verbose {
		log.Printf("[%s] IMAP >>> %s %s", ic.sessionID, tag, cmd)
	}

	if _, err := fmt.Fprintf(ic.conn, "%s %s\r\n", tag, cmd); err != nil {
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
			return imapResult{}, fmt.Errorf("read IMAP response: %v", err)
		}

		if strings.HasPrefix(line, tag+" ") {
			if verbose {
				log.Printf("[%s] IMAP <<< %s", ic.sessionID, line)
			}
			return imapResult{lines: lines, literals: literals, tagged: line}, nil
		}

		if verbose {
			log.Printf("[%s] IMAP <<< %s", ic.sessionID, line)
			if literal != "" {
				log.Printf("[%s] IMAP <<< [literal %d bytes]", ic.sessionID, len(literal))
			}
		}
		idx := len(lines)
		lines = append(lines, line)
		if literal != "" {
			literals[idx] = literal
		}
	}
}

func (ic *ImapConnection) readLine() (string, string, error) {
	line, err := ic.reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	line = strings.TrimRight(line, "\r\n")

	m := reLiteral.FindStringSubmatch(line)
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

	line = reLiteral.ReplaceAllString(line, "") + afterLine
	return line, literal, nil
}

func (ic *ImapConnection) login(username, password string) error {
	result, err := ic.sendCommand(fmt.Sprintf(`LOGIN "%s" "%s"`, imapEscape(username), imapEscape(password)))
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("IMAP LOGIN failed: %s", result.tagged)
	}
	log.Printf("[%s] LOGIN OK (%s)", ic.sessionID, username)
	return nil
}

func (ic *ImapConnection) selectInbox() error {
	result, err := ic.sendCommand("SELECT INBOX")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("IMAP SELECT failed: %s", result.tagged)
	}

	ic.messageSizes = make(map[int]int)
	ic.messageUids = make(map[int]string)
	ic.deleted = make(map[int]bool)
	ic.messageCount = 0

	for _, line := range result.lines {
		m := reExists.FindStringSubmatch(line)
		if m != nil {
			ic.messageCount, _ = strconv.Atoi(m[1])
		}
	}
	log.Printf("[%s] SELECT OK, %d messages", ic.sessionID, ic.messageCount)
	return nil
}

// Combined: fetch sizes and UIDs in one IMAP command
func (ic *ImapConnection) fetchSizesAndUids() error {
	result, err := ic.sendCommand("FETCH 1:* (RFC822.SIZE UID)")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return fmt.Errorf("FETCH sizes+UIDs failed: %s", result.tagged)
	}

	for _, line := range result.lines {
		seqM := reFetchSeq.FindStringSubmatch(line)
		if seqM == nil {
			continue
		}
		seq, _ := strconv.Atoi(seqM[1])

		sm := reSizeField.FindStringSubmatch(line)
		if sm != nil {
			ic.messageSizes[seq], _ = strconv.Atoi(sm[1])
		}

		um := reUidField.FindStringSubmatch(line)
		if um != nil {
			ic.messageUids[seq] = um[1]
		}
	}
	log.Printf("[%s] FETCH sizes+UIDs OK, %d sizes, %d UIDs", ic.sessionID, len(ic.messageSizes), len(ic.messageUids))
	return nil
}

const headerBatchSize = 50

func (ic *ImapConnection) fetchHeadersWithCache(seqNum int) (string, error) {
	if h, ok := ic.messageHdrs[seqNum]; ok {
		log.Printf("[%s] headers #%d cached, %d bytes", ic.sessionID, seqNum, len(h))
		return h, nil
	}

	start := seqNum
	end := seqNum + headerBatchSize - 1
	if end > ic.messageCount {
		end = ic.messageCount
	}
	if end - start + 1 < headerBatchSize {
		start = end - headerBatchSize + 1
		if start < 1 {
			start = 1
		}
	}

	result, err := ic.sendCommand(fmt.Sprintf("FETCH %d:%d (BODY[HEADER])", start, end))
	if err != nil {
		return ic.fetchHeadersSingle(seqNum)
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return ic.fetchHeadersSingle(seqNum)
	}

	cached := 0
	for i, line := range result.lines {
		m := reFetchSeq.FindStringSubmatch(line)
		if m != nil {
			seq, _ := strconv.Atoi(m[1])
			if lit, ok := result.literals[i]; ok {
				ic.messageHdrs[seq] = lit
				cached++
			}
		}
	}
	log.Printf("[%s] batch headers %d:%d, cached %d", ic.sessionID, start, end, cached)

	h, ok := ic.messageHdrs[seqNum]
	if !ok {
		return ic.fetchHeadersSingle(seqNum)
	}
	return h, nil
}

func (ic *ImapConnection) fetchHeadersSingle(seqNum int) (string, error) {
	result, err := ic.sendCommand(fmt.Sprintf("FETCH %d (BODY[HEADER])", seqNum))
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return "", fmt.Errorf("FETCH headers failed: %s", result.tagged)
	}
	h := ic.getLiteralForSeq(result, seqNum)
	ic.messageHdrs[seqNum] = h
	log.Printf("[%s] FETCH headers #%d, %d bytes", ic.sessionID, seqNum, len(h))
	return h, nil
}

func (ic *ImapConnection) fetchTop(seqNum int, lineCount int) (string, error) {
	headers, err := ic.fetchHeadersWithCache(seqNum)
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
	log.Printf("[%s] TOP #%d, %d body lines", ic.sessionID, seqNum, len(filtered))
	return headers + "\r\n" + strings.Join(filtered, "\r\n"), nil
}

// fetchMessageWithCache: check cache first, fetch from IMAP if not cached
func (ic *ImapConnection) fetchMessageWithCache(seqNum int) (string, error) {
	if msg, ok := ic.messageCache[seqNum]; ok {
		log.Printf("[%s] RETR #%d cached, %d bytes", ic.sessionID, seqNum, len(msg))
		return msg, nil
	}
	return ic.fetchMessageSingle(seqNum)
}

func (ic *ImapConnection) fetchMessageSingle(seqNum int) (string, error) {
	result, err := ic.sendCommand(fmt.Sprintf("FETCH %d (RFC822)", seqNum))
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToUpper(result.tagged), "OK") {
		return "", fmt.Errorf("FETCH message failed: %s", result.tagged)
	}
	msg := ic.getLiteralForSeq(result, seqNum)
	ic.messageCache[seqNum] = msg
	log.Printf("[%s] FETCH #%d, %d bytes", ic.sessionID, seqNum, len(msg))
	return msg, nil
}

// prefetchNextMessages: proactively fetch the next few messages after RETR
// so they're cached when the POP3 client requests them
func (ic *ImapConnection) prefetchNextMessages(afterSeqNum int) {
	pipelineDepth := 3
	var seqNums []int
	var cmds []string
	var buf strings.Builder

	for i := 1; i <= pipelineDepth; i++ {
		n := afterSeqNum + i
		if n > ic.messageCount {
			break
		}
		if _, ok := ic.messageCache[n]; ok {
			continue // already cached
		}
		seqNums = append(seqNums, n)
		tag := ic.nextTag()
		cmds = append(cmds, tag)
		fmt.Fprintf(&buf, "%s FETCH %d (RFC822)\r\n", tag, n)
	}

	if len(cmds) == 0 {
		return
	}

	if verbose {
		for i, n := range seqNums {
			log.Printf("[%s] IMAP >>> %s FETCH %d (RFC822)", ic.sessionID, cmds[i], n)
		}
	}

	if _, err := ic.conn.Write([]byte(buf.String())); err != nil {
		log.Printf("[%s] prefetch send failed: %v", ic.sessionID, err)
		return
	}

	cached := 0
	for i, tag := range cmds {
		result, err := ic.readResponse(tag)
		if err != nil {
			log.Printf("[%s] prefetch #%d failed: %v", ic.sessionID, seqNums[i], err)
			continue
		}
		if strings.Contains(strings.ToUpper(result.tagged), "OK") {
			msg := ic.getLiteralForSeq(result, seqNums[i])
			ic.messageCache[seqNums[i]] = msg
			cached++
		}
	}
	log.Printf("[%s] prefetch after #%d, cached %d/%d", ic.sessionID, afterSeqNum, cached, len(cmds))
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
	count := len(ic.deleted)
	for seqNum := range ic.deleted {
		_, _ = ic.sendCommand(fmt.Sprintf("STORE %d -FLAGS (\\Deleted)", seqNum))
	}
	ic.deleted = make(map[int]bool)
	log.Printf("[%s] RSET OK, cleared %d deletions", ic.sessionID, count)
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
	log.Printf("[%s] IMAP closed", ic.sessionID)
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