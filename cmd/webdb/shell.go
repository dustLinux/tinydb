// Подкоманда `webdb shell` — интерактивная консоль к tinydb, по духу sqlite3.
//
//	$ webdb shell
//	tinydb> .tables
//	tinydb> SELECT count(*) AS n FROM docs;
//	tinydb> put users u1 {"name":"alice"}
//	tinydb> ls users
//
// Живёт внутри того же бинаря, что и сервер: отдельная программа тащила бы
// с собой ещё одну копию HTTP-клиента, а здесь они делят и код, и тесты. На
// размер бинаря сервера пофиг (важна память), а памяти консоль серверу не
// добавляет — это другой кодовый путь, его страницы не трогаются.
//
// Работает поверх HTTP API, поэтому умеет всё, что умеет сервер, и ровно тем же
// способом (никакого «локального режима» с другими правилами). Токен берётся
// из -token, из env WEBDB_TOKEN или из <data>/token.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustlinux/tinydb/internal/httpx"
	"github.com/dustlinux/tinydb/internal/server"
)

type client struct {
	hc     *httpx.Client
	token  string
	header bool   // .headers on
	mode   string // column | list | json
}

// runShell — точка входа подкоманды `webdb shell`.
func runShell(args []string) int {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	var (
		addr    = fs.String("addr", "127.0.0.1:8080", "server address")
		token   = fs.String("token", "", "API token (or env WEBDB_TOKEN; default: <data>/token)")
		dataDir = fs.String("data", "./data", "data dir to read the token from")
		timeout = fs.Duration("timeout", 30*time.Second, "request timeout")
		exec    = fs.String("cmd", "", "run one command and exit")
		version = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `webdb shell — консоль tinydb (в духе sqlite3)

usage:
  webdb shell [flags]              интерактивный режим
  webdb shell -cmd ".tables"       одна команда и выход
  echo "SELECT 1" | webdb shell    чтение команд из stdin

Флаги:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Команды (точка — служебная, остальное — SQL к /v1/query):
  .help            эта справка        .quit / .exit   выход
  .tables          коллекции          .schema [coll]  поля и индексы
  .headers on|off  заголовки          .mode col|list|json  формат вывода
  .token           какой токен используется (маскируется)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *version {
		fmt.Println("webdb shell " + server.Version + " (tinydb)")
		return 0
	}

	tok := *token
	if tok == "" {
		tok = os.Getenv("WEBDB_TOKEN")
	}
	if tok == "" {
		if b, err := os.ReadFile(*dataDir + "/token"); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}

	c := &client{
		hc:    httpx.NewClient(httpx.ClientConfig{Addr: *addr, Token: tok, Timeout: *timeout}),
		token: tok,
		mode:  "column",
	}

	if *exec != "" {
		if err := c.execLine(*exec); err != nil {
			fmt.Fprintln(os.Stderr, "webdb shell:", err)
			return 1
		}
		return 0
	}

	fmt.Printf("tinydb %s — Ctrl-D или .quit для выхода\n", *addr)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var pending strings.Builder
	prompt := "tinydb> "
	for {
		fmt.Print(prompt)
		if !in.Scan() {
			fmt.Println()
			return 0
		}
		line := in.Text()
		// Многострочный ввод: незакрытая кавычка/скобка ждёт продолжения.
		if pending.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
		}
		pending.WriteString(line)
		pending.WriteString("\n")
		joined := pending.String()
		if !complete(joined) {
			prompt = "   ...> "
			continue
		}
		prompt = "tinydb> "
		pending.Reset()
		if err := safeExec(c, strings.TrimRight(joined, "\n")); err != nil {
			if err == errExit {
				return 0
			}
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

// safeExec — REPL не должен умирать из-за одной плохой команды: ловим панику
// и превращаем её в сообщение.
func safeExec(c *client, line string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("внутренняя ошибка: %v", r)
		}
	}()
	return c.execLine(line)
}

var errExit = fmt.Errorf("exit")

// complete — грубая эвристика «закрыт ли ввод»: считаем кавычки и скобки.
func complete(s string) bool {
	inS, inD, esc := false, false, false
	depth := 0
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inS:
			esc = true
		case r == '\'' && !inD:
			inS = !inS
		case r == '"' && !inS:
			inD = !inD
		case inS || inD:
			// внутри строк скобки не считаем
		case r == '(' || r == '[' || r == '{':
			depth++
		case r == ')' || r == ']' || r == '}':
			depth--
		}
	}
	return !inS && !inD && depth <= 0
}

func (c *client) execLine(line string) error {
	line = strings.TrimSpace(stripComment(line))
	if line == "" {
		return nil
	}
	if strings.HasPrefix(line, ".") {
		return c.dotCommand(line)
	}
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "select"), strings.HasPrefix(lower, "with"),
		strings.HasPrefix(lower, "explain"):
		return c.doSQL(strings.TrimSuffix(strings.TrimSpace(line), ";"))
	case strings.HasPrefix(lower, "insert"), strings.HasPrefix(lower, "update"),
		strings.HasPrefix(lower, "delete"), strings.HasPrefix(lower, "drop"),
		strings.HasPrefix(lower, "create"), strings.HasPrefix(lower, "alter"),
		strings.HasPrefix(lower, "attach"), strings.HasPrefix(lower, "pragma"),
		strings.HasPrefix(lower, "vacuum"):
		fmt.Println("error: /v1/query — только чтение. Для изменений: put / rm / .index")
		return nil
	case strings.HasPrefix(lower, "ls "), lower == "ls", strings.HasPrefix(lower, "select * from "):
		if lower == "ls" {
			return c.cmdLs("", 20)
		}
		if strings.HasPrefix(lower, "ls ") {
			return c.cmdLs(strings.TrimSpace(lower[3:]), 20)
		}
	}
	// Короткие команды: get/put/rm — приятное сокращение для часто повторяющихся вещей.
	f := strings.Fields(line)
	switch strings.ToLower(f[0]) {
	case "get":
		if len(f) < 3 {
			return usage("get COLL ID")
		}
		return c.cmdGet(f[1], f[2])
	case "put":
		if len(f) < 4 {
			return usage(`put COLL ID '{"json":"here"}'`)
		}
		return c.cmdPut(f[1], f[2], strings.Join(f[3:], " "), true)
	case "rm", "del":
		if len(f) < 3 {
			return usage("rm COLL ID")
		}
		return c.cmdDel(f[1], f[2])
	case "idx", "index":
		return c.cmdIndex(f[1:])
	}
	// Многословные: help/quit/exit/stats/flush/export/tables/schema/token
	switch strings.ToLower(strings.Join(f, " ")) {
	case "help", "?":
		c.help()
		return nil
	case "quit", "exit":
		return errExit
	case "stats", "stat":
		return c.cmdStats()
	case "flush":
		_, err := c.req("POST", "/v1/flush", nil)
		return err
	case "export":
		b, err := c.reqBytes("GET", "/v1/export", nil)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("неизвестная команда: %q (.help — список)", line)
}

func stripComment(s string) string {
	inS, inD, esc := false, false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inS:
			esc = true
		case ch == '\'' && !inD:
			inS = !inS
		case ch == '"' && !inS:
			inD = !inD
		case ch == '-' && i+1 < len(s) && s[i+1] == '-' && !inS && !inD:
			return s[:i]
		}
	}
	return s
}

func usage(s string) error { return fmt.Errorf("usage: %s", s) }

// ---- HTTP ----

func (c *client) req(method, path string, body []byte) (map[string]any, error) {
	b, err := c.reqBytes(method, path, body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("плохой ответ сервера: %w", err)
		}
	}
	return m, nil
}

// reqBytes — один запрос целиком в память (для коротких ответов).
func (c *client) reqBytes(method, path string, body []byte) ([]byte, error) {
	resp, err := c.hc.Do(method, path, body)
	if err != nil {
		if err == httpx.ErrAuth {
			return nil, err
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(resp.Body)
	if resp.Status >= 400 {
		var e struct {
			Error  string `json:"error"`
			Status int    `json:"status"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.Status, msg)
	}
	if readErr != nil {
		return nil, readErr
	}
	return b, nil
}

// ---- служебные команды ----

func (c *client) dotCommand(line string) error {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]
	switch cmd {
	case ".help", ".?":
		c.help()
	case ".quit", ".exit":
		return errExit
	case ".tables":
		return c.cmdTables()
	case ".schema":
		coll := ""
		if len(args) > 0 {
			coll = args[0]
		}
		return c.cmdSchema(coll)
	case ".headers":
		if len(args) == 0 {
			return fmt.Errorf("usage: .headers on|off (сейчас %v)", c.header)
		}
		switch strings.ToLower(args[0]) {
		case "on":
			c.header = true
		case "off":
			c.header = false
		default:
			return fmt.Errorf(".headers on|off")
		}
	case ".mode":
		if len(args) == 0 {
			return fmt.Errorf("usage: .mode column|list|json (сейчас %s)", c.mode)
		}
		switch strings.ToLower(args[0]) {
		case "column", "col":
			c.mode = "column"
		case "list":
			c.mode = "list"
		case "json":
			c.mode = "json"
		default:
			return fmt.Errorf(".mode column|list|json")
		}
	case ".token":
		if c.token == "" {
			fmt.Println("токен не задан (сервер с -no-auth?)")
			return nil
		}
		if len(c.token) <= 12 {
			fmt.Println("токен:", c.token)
		} else {
			fmt.Printf("токен: %s…%s (%d симв.)\n", c.token[:4], c.token[len(c.token)-4:], len(c.token))
		}
	case ".index", ".indexes":
		return c.cmdIndex(args)
	case ".stats":
		return c.cmdStats()
	case ".flush":
		_, err := c.req("POST", "/v1/flush", nil)
		return err
	case ".export":
		b, err := c.reqBytes("GET", "/v1/export", nil)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	case ".ls":
		coll, n := "", 20
		if len(args) > 0 {
			coll = args[0]
		}
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil {
				n = v
			}
		}
		return c.cmdLs(coll, n)
	case ".get":
		if len(args) < 2 {
			return usage(".get COLL ID")
		}
		return c.cmdGet(args[0], args[1])
	case ".put":
		if len(args) < 3 {
			return usage(`.put COLL ID '{"json":"here"}'`)
		}
		return c.cmdPut(args[0], args[1], strings.Join(args[2:], " "), true)
	case ".rm", ".del":
		if len(args) < 2 {
			return usage(".rm COLL ID")
		}
		return c.cmdDel(args[0], args[1])
	case ".version":
		fmt.Println("webdb shell " + server.Version + " (tinydb)")
	default:
		return fmt.Errorf("неизвестная команда %q (.help)", cmd)
	}
	return nil
}

func (c *client) help() {
	fmt.Print(`tinydb — консоль

  SQL (read-only)        SELECT ...;  WITH ...;  EXPLAIN ...
  Служебные              .help  .quit  .tables  .schema [coll]  .headers on|off
                         .mode column|list|json  .token  .index [list|add FIELD|del FIELD]
  Коллекции              ls [coll] [N]     get COLL ID
  Документы              put COLL ID '{json}'    rm COLL ID
  Сервер                 stats   flush   export

Примеры:
  .tables
  ls items 5
  put items it1 '{"title":"гайка","price":10}'
  SELECT collection, count(*) FROM docs GROUP BY collection;
`)
}

// ---- вывод результатов ----

func (c *client) printRows(cols []string, rows [][]any) {
	if len(rows) == 0 {
		fmt.Println("(0 строк)")
		return
	}
	switch c.mode {
	case "json":
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			m := map[string]any{}
			for i, col := range cols {
				if i < len(r) {
					m[col] = r[i]
				}
			}
			out = append(out, m)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case "list":
		for _, r := range rows {
			parts := make([]string, 0, len(r))
			for i := range cols {
				if i < len(r) {
					parts = append(parts, cell(r[i]))
				}
			}
			fmt.Println(strings.Join(parts, " | "))
		}
	default: // column
		widths := make([]int, len(cols))
		for i, col := range cols {
			widths[i] = len(col)
		}
		for _, r := range rows {
			for i := range cols {
				if i < len(r) {
					if n := len(cell(r[i])); n > widths[i] {
						widths[i] = n
					}
				}
			}
		}
		printRow(cols, widths)
		if c.header {
			sep := make([]string, len(cols))
			for i, w := range widths {
				sep[i] = strings.Repeat("-", w)
			}
			printRow(sep, widths)
		}
		for _, r := range rows {
			cells := make([]string, len(cols))
			for i := range cols {
				if i < len(r) {
					cells[i] = cell(r[i])
				}
			}
			printRow(cells, widths)
		}
	}
}

// cell — представление значения для вывода: map'ы и слайсы печатаем как JSON,
// иначе пользователь видит «map[price:10 title:гайка]».
func cell(v any) string {
	switch v.(type) {
	case map[string]any, []any:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(v)
}

func printRow(cells []string, widths []int) {
	var b strings.Builder
	for i, c := range cells {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(c)
		if i < len(cells)-1 && i < len(widths) {
			if pad := widths[i] - len(c); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
	}
	fmt.Println(b.String())
}

// printHeader печатает заголовки колонок с подчёркиванием (для .headers on).
func printHeader(cols []string) {
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = len(c)
	}
	printRow(cols, w)
	sep := make([]string, len(cols))
	for i := range sep {
		sep[i] = strings.Repeat("-", w[i])
	}
	printRow(sep, w)
}

func (c *client) doSQL(sql string) error {
	b, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		return err
	}
	m, err := c.req("POST", "/v1/query", b)
	if err != nil {
		return err
	}
	cols := toStrings(m["columns"])
	rawRows, _ := m["rows"].([]any)
	// Сервер отдаёт строки объектами {"col": value, ...} — приводим к
	// значениям в порядке cols (на случай другого формата держим и массивы).
	rows := make([][]any, 0, len(rawRows))
	for _, rr := range rawRows {
		switch v := rr.(type) {
		case map[string]any:
			row := make([]any, len(cols))
			for i, col := range cols {
				row[i] = v[col]
			}
			rows = append(rows, row)
		case []any:
			rows = append(rows, v)
		}
	}
	c.printRows(cols, rows)
	return nil
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// ---- команды над коллекциями ----

func (c *client) cmdTables() error {
	m, err := c.req("GET", "/v1/collections", nil)
	if err != nil {
		return err
	}
	arr, _ := m["collections"].([]any)
	if len(arr) == 0 {
		fmt.Println("(нет коллекций)")
		return nil
	}
	names := make([]string, 0, len(arr))
	docs := map[string]any{}
	for _, x := range arr {
		if cm, ok := x.(map[string]any); ok {
			n := fmt.Sprint(cm["name"])
			names = append(names, n)
			docs[n] = cm["docs"]
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("%-24s %v доков\n", n, docs[n])
	}
	return nil
}

func (c *client) cmdSchema(coll string) error {
	if coll == "" {
		return c.cmdTables()
	}
	idx, err := c.req("GET", "/v1/collections/"+coll+"/indexes", nil)
	if err != nil {
		return err
	}
	names := toStrings(idx["indexes"])
	fmt.Printf("CREATE TABLE %s (id TEXT PRIMARY KEY, created INT, updated INT, data JSON);\n", coll)
	if len(names) == 0 {
		fmt.Println("-- индексов нет")
	} else {
		for _, n := range names {
			fmt.Printf("CREATE INDEX %s;\n", n)
		}
	}
	// Показать реальные поля одного документа — полезнее, чем угадывать.
	m, err := c.req("GET", "/v1/collections/"+coll+"/docs?limit=1", nil)
	if err != nil {
		return nil // пустая коллекция — не ошибка
	}
	if items, ok := m["items"].([]any); ok && len(items) > 0 {
		if d, ok := items[0].(map[string]any); ok {
			if data, ok := d["data"].(map[string]any); ok {
				keys := make([]string, 0, len(data))
				for k := range data {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Printf("-- поле: %s (%T)\n", k, data[k])
				}
			}
		}
	}
	return nil
}

func (c *client) cmdLs(coll string, limit int) error {
	if coll == "" {
		return c.cmdTables()
	}
	if n, err := strconv.Atoi(coll); err == nil {
		limit = n
		coll = ""
	}
	path := "/v1/collections"
	if coll != "" {
		path += "/" + coll + "/docs?limit=" + strconv.Itoa(limit)
	} else {
		path += "?limit=" + strconv.Itoa(limit)
	}
	m, err := c.req("GET", path, nil)
	if err != nil {
		return err
	}
	items, _ := m["items"].([]any)
	if len(items) == 0 {
		fmt.Println("(пусто)")
		return nil
	}
	cols := []string{"id", "collection", "updated", "data"}
	if c.header {
		printHeader(cols)
	}
	for _, it := range items {
		d, ok := it.(map[string]any)
		if !ok {
			continue
		}
		data, _ := json.Marshal(d["data"])
		s := string(data)
		if len(s) > 120 {
			s = s[:117] + "..."
		}
		fmt.Printf("%-16v %-12v %-8v %s\n", d["_id"], d["collection"], d["updated"], s)
	}
	if t, ok := m["total"]; ok {
		fmt.Printf("(%v всего)\n", t)
	}
	return nil
}

func (c *client) cmdGet(coll, id string) error {
	m, err := c.req("GET", "/v1/collections/"+coll+"/docs/"+id, nil)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	return nil
}

func (c *client) cmdPut(coll, id, body string, upsert bool) error {
	var probe any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return fmt.Errorf("невалидный JSON: %w", err)
	}
	p := "/v1/collections/" + coll + "/docs/" + id
	if upsert {
		p += "?upsert=true"
	}
	_, err := c.req("PUT", p, []byte(body))
	return err
}

func (c *client) cmdDel(coll, id string) error {
	_, err := c.req("DELETE", "/v1/collections/"+coll+"/docs/"+id, nil)
	return err
}

func (c *client) cmdIndex(args []string) error {
	coll, field := "", ""
	if len(args) == 0 {
		return usage("index list|add FIELD|del FIELD")
	}
	sub := strings.ToLower(args[0])
	switch sub {
	case "add", "create", "new":
		if len(args) < 2 {
			return usage("index add FIELD")
		}
		if len(args) >= 3 {
			coll = args[1]
			field = args[2]
		}
		if field == "" {
			return fmt.Errorf("нужно указать поле")
		}
	default:
		// `.index COLL` — показать индексы коллекции; `.index list COLL` — то же;
		// `.index add|del COLL FIELD` — создать/удалить.
		switch sub {
		case "add", "create", "new", "del", "drop", "rm":
			if len(args) >= 3 {
				coll, field = args[1], args[2]
			} else if len(args) == 2 {
				coll = args[1]
			} else {
				return usage("index " + sub + " COLL FIELD")
			}
		default:
			// Неизвестное слово считаем именем коллекции: `.index items`.
			if len(args) >= 2 {
				coll = args[1]
			} else if len(args) == 1 {
				coll = args[0]
			}
		}
	}
	if coll == "" && field != "" {
		return fmt.Errorf("укажите коллекцию: index %s COLL %s", sub, field)
	}
	switch sub {
	case "add", "create", "new":
		_, err := c.req("POST", "/v1/collections/"+coll+"/indexes?field="+field, nil)
		return err
	case "del", "drop", "rm":
		if field == "" {
			return usage("index del COLL FIELD")
		}
		_, err := c.req("DELETE", "/v1/collections/"+coll+"/indexes/"+field, nil)
		return err
	default:
		// `.index`, `.index COLL`, `.index list` — показать индексы.
		if coll == "" {
			return c.cmdTables()
		}
		m, err := c.req("GET", "/v1/collections/"+coll+"/indexes", nil)
		if err != nil {
			return err
		}
		list := toStrings(m["indexes"])
		if len(list) == 0 {
			fmt.Printf("(у %s индексов нет)\n", coll)
			return nil
		}
		for _, n := range list {
			fmt.Println(n)
		}
		return nil
	}
}

func (c *client) cmdStats() error {
	m, err := c.req("GET", "/v1/stats", nil)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%-20s %v\n", k, m[k])
	}
	return nil
}
