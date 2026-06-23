package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha512"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

var memcacheClient *memcache.Client

// アプリ起動時に一度だけ実行される初期化処理。Memcacheクライアントとセッションストアをセットアップする。
func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

// ベンチマーク開始前にDBをリセットする。余分なデータを削除し、一部ユーザーをBANフラグ付きにする。
func dbInitialize(ctx context.Context) {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}
}

// アカウント名とパスワードでログインを試みる。認証成功ならUserを返し、失敗ならnilを返す。
func tryLogin(ctx context.Context, accountName, password string) *User {
	u := User{}
	err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

// アカウント名（3文字以上の英数字_）とパスワード（6文字以上の英数字_）の形式を検証する。
func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// 文字列をSHA-512でハッシュ化して16進数文字列で返す。パスワード保護に使う。
func digest(src string) string {
	h := sha512.New()
	h.Write([]byte(src))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// アカウント名からソルト（パスワードハッシュに混ぜる値）を生成する。同じパスワードでもハッシュが一致しないようにするため。
func calculateSalt(accountName string) string {
	return digest(accountName)
}

// パスワードとソルトを組み合わせてDBに保存するハッシュ値を生成する。
func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

// リクエストからMemcache上のセッションデータを取得する。
func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

// セッションのuser_idからDBを引いてログイン中のユーザーを返す。未ログインなら空のUserを返す。
func getSessionUser(r *http.Request) User {
	ctx := r.Context()
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	err := db.GetContext(ctx, &u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}

	return u
}

// セッションから一度だけ表示するメッセージ（フラッシュメッセージ）を取り出して返す。取り出したら削除される。
func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

// 投稿リストにコメント数・コメント・ユーザー情報を付加して返す。N+1を避けるため関連データをIN句で一括取得している。
// allCommentsがfalseのときは各投稿のコメントを最新3件に絞る（トップページ用）。trueなら全件（詳細ページ用）。
func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	if len(results) == 0 {
		return nil, nil
	}

	// Step1: 全投稿IDを集める
	postIDs := make([]int, len(results))
	for i, p := range results {
		postIDs[i] = p.ID
	}

	// Step2: コメント数を一括取得（20回 → 1回）
	type commentCount struct {
		PostID int `db:"post_id"`
		Count  int `db:"count"`
	}
	var counts []commentCount
	q, args, err := sqlx.In("SELECT `post_id`, COUNT(*) AS `count` FROM `comments` WHERE `post_id` IN (?) GROUP BY `post_id`", postIDs)
	if err != nil {
		return nil, err
	}
	if err := db.SelectContext(ctx, &counts, q, args...); err != nil {
		return nil, err
	}
	commentCountMap := make(map[int]int, len(counts))
	for _, c := range counts {
		commentCountMap[c.PostID] = c.Count
	}

	// Step3: コメントを一括取得（20回 → 1回）
	var allCommentList []Comment
	q, args, err = sqlx.In("SELECT * FROM `comments` WHERE `post_id` IN (?) ORDER BY `post_id`, `created_at` DESC", postIDs)
	if err != nil {
		return nil, err
	}
	if err := db.SelectContext(ctx, &allCommentList, q, args...); err != nil {
		return nil, err
	}

	// Step4: post_idごとに振り分け（!allCommentsなら3件まで）
	commentMap := make(map[int][]Comment)
	for _, c := range allCommentList {
		if allComments || len(commentMap[c.PostID]) < 3 {
			commentMap[c.PostID] = append(commentMap[c.PostID], c)
		}
	}

	// Step5: 必要なユーザーIDを全部集める（重複なし）
	userIDSet := make(map[int]struct{})
	for _, p := range results {
		userIDSet[p.UserID] = struct{}{}
	}
	for _, comments := range commentMap {
		for _, c := range comments {
			userIDSet[c.UserID] = struct{}{}
		}
	}
	userIDs := make([]int, 0, len(userIDSet))
	for id := range userIDSet {
		userIDs = append(userIDs, id)
	}

	// Step6: ユーザーを一括取得（最大80回 → 1回）
	var users []User
	q, args, err = sqlx.In("SELECT * FROM `users` WHERE `id` IN (?)", userIDs)
	if err != nil {
		return nil, err
	}
	if err := db.SelectContext(ctx, &users, q, args...); err != nil {
		return nil, err
	}
	userMap := make(map[int]User, len(users))
	for _, u := range users {
		userMap[u.ID] = u
	}

	// Step7: 組み立て（DBアクセスなし）
	var posts []Post
	for _, p := range results {
		p.CommentCount = commentCountMap[p.ID]

		comments := commentMap[p.ID]
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}
		for i := range comments {
			comments[i].User = userMap[comments[i].UserID]
		}
		p.Comments = comments

		p.User = userMap[p.UserID]
		p.CSRFToken = csrfToken

		if p.User.DelFlg == 0 {
			posts = append(posts, p)
		}
		if len(posts) >= postsPerPage {
			break
		}
	}

	return posts, nil
}

// 投稿のMIMEタイプから拡張子を決め、静的ファイルのURLパスを返す（例: /image/123.jpg）。
func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

// IDが0かどうかでログイン済みかを判定する（未ログインの場合はUser{}が返るためID=0になる）。
func isLogin(u User) bool {
	return u.ID != 0
}

// セッションからCSRFトークンを取得する。フォーム送信時の正規性チェックに使う。
func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

// 暗号的に安全なbバイトのランダム文字列を生成する。CSRFトークンの発行に使う。
func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

// テンプレートファイルのパスを返すヘルパー（templates/ディレクトリからの相対パス）。
func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

var templates map[string]*template.Template

func initTemplates() {
	fmap := template.FuncMap{"imageURL": imageURL}
	templates = map[string]*template.Template{
		"login":    template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("login.html"))),
		"register": template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("register.html"))),
		"banned":   template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("banned.html"))),
		"index":    template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("index.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"user":     template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("user.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"posts":    template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(getTemplPath("posts.html"), getTemplPath("post.html"))),
		"post_id":  template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("post_id.html"), getTemplPath("post.html"))),
	}
}

// GET /initialize — ベンチマーク開始時に呼ばれる。DBを初期状態にリセットして200を返す。
func getInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbInitialize(ctx)
	w.WriteHeader(http.StatusOK)
}

// GET /login — ログインページを表示する。すでにログイン済みならトップページへリダイレクト。
func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templates["login"].Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

// POST /login — フォームのaccount_nameとpasswordで認証し、成功ならセッションを作成してトップページへ。
func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// GET /register — ユーザー登録ページを表示する。すでにログイン済みならトップページへリダイレクト。
func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	templates["register"].Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

// POST /register — アカウント名・パスワードを受け取り、バリデーション・重複確認を経てユーザーを作成する。
func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

// GET /logout — セッションを破棄してトップページへリダイレクトする。
func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

// GET / — トップページ。全投稿を新着順で取得し、makePosts経由でコメント・ユーザー情報を付けてHTMLを返す。
func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)

	results := []Post{}

	err := db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` ORDER BY `created_at` DESC LIMIT 20")
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	templates["index"].Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

// GET /@{accountName} — ユーザープロフィールページ。そのユーザーの投稿一覧・投稿数・コメント数・被コメント数を表示する。
func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT 20", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.SelectContext(ctx, &postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []any
		args := make([]any, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.GetContext(ctx, &commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	templates["user"].Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

// GET /posts?max_created_at=... — 無限スクロール用。指定日時以前の投稿をHTMLフラグメントで返す。
func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `created_at` <= ? ORDER BY `created_at` DESC LIMIT 20", t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	templates["posts"].Execute(w, posts)
}

// GET /posts/{id} — 投稿詳細ページ。コメントを全件表示する（makePosts に allComments=true を渡す）。
func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	templates["post_id"].Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

// POST / — 新規投稿を作成する。画像ファイルをバリデーションしてDBに保存し、public/imageへファイルとして書き出す。
func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `body`) VALUES (?,?,?)"
	result, err := db.ExecContext(ctx, query, me.ID, mime, r.FormValue("body"))

	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	ext := ""
	switch mime {
	case "image/jpeg":
		ext = "jpg"
	case "image/png":
		ext = "png"
	case "image/gif":
		ext = "gif"
	}
	imgPath := fmt.Sprintf("../public/image/%d.%s", pid, ext)
	if err := os.WriteFile(imgPath, filedata, 0644); err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

// POST /comment — ログイン中のユーザーが指定投稿にコメントを追加する。
func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.ExecContext(ctx, query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

// GET /admin/banned — 管理者専用。BANされていない一般ユーザー一覧を表示する。非管理者は403を返す。
func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	templates["banned"].Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

// POST /admin/banned — 管理者専用。選択されたユーザーのdel_flgを1にしてBANする。
func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

// アプリのエントリーポイント。DB接続を確立し、ルーティングを設定して:8080でHTTPサーバーを起動する。
func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{
		"charset": "utf8mb4",
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	dsn := cfg.FormatDSN()

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}

	initTemplates()
	defer db.Close()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir("../public")))

	log.Fatal(http.ListenAndServe(":8080", r))
}
