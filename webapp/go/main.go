package main

import (
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
)

const (
	sessionName            = "isucondition"
	searchLimit            = 20
	notificationLimit      = 20
	isuListLimit           = 200 // TODO 修正が必要なら変更
	jwtVerificationKeyPath = "../ec256-public.pem"
	DefaultJIAServiceURL   = "http://localhost:5000"
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jwtVerificationKey *ecdsa.PublicKey
)

type Config struct {
	Name string `db:"name"`
	URL  string `db:"url"`
}

type Isu struct {
	JIAIsuUUID   string    `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name         string    `db:"name" json:"name"`
	Image        []byte    `db:"image" json:"-"`
	JIACatalogID string    `db:"jia_catalog_id" json:"jia_catalog_id"`
	Character    string    `db:"character" json:"character"`
	JIAUserID    string    `db:"jia_user_id" json:"-"`
	IsDeleted    bool      `db:"is_deleted" json:"-"`
	CreatedAt    time.Time `db:"created_at" json:"-"`
	UpdatedAt    time.Time `db:"updated_at" json:"-"`
}

type CatalogFromJIA struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	LimitWeight int    `json:"limit_weight"`
	Weight      int    `json:"weight"`
	Size        string `json:"size"`
	Maker       string `json:"maker"`
	Features    string `json:"features"`
}

type Catalog struct {
	JIACatalogID string `json:"jia_catalog_id"`
	Name         string `json:"name"`
	LimitWeight  int    `json:"limit_weight"`
	Weight       int    `json:"weight"`
	Size         string `json:"size"`
	Maker        string `json:"maker"`
	Tags         string `json:"tags"`
}

type IsuLog struct {
	JIAIsuUUID string    `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Timestamp  time.Time `db:"timestamp" json:"timestamp"`
	Condition  string    `db:"condition" json:"condition"`
	Message    string    `db:"message" json:"message"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

//グラフ表示用  一時間のsummry 詳細
type GraphData struct {
	Score   int            `json:"score"`
	Sitting int            `json:"sitting"`
	Detail  map[string]int `json:"detail"`
}

//グラフ表示用  一時間のsummry
type Graph struct {
	JIAIsuUUID string    `db:"jia_isu_uuid"`
	StartAt    time.Time `db:"start_at"`
	Data       string    `db:"data"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

type User struct {
	JIAUserID string    `db:"jia_user_id"`
	CreatedAt time.Time `db:"created_at"`
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
}

type NotificationResponse struct {
}

type PutIsuRequest struct {
	Name string `json:"name"`
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

//ConnectDB データベースに接続する
func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Local", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName)
	return sqlx.Open("mysql", dsn)
}

func init() {
	sessionStore = sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))

	key, err := ioutil.ReadFile(jwtVerificationKeyPath)
	if err != nil {
		log.Fatalf("Unable to read file: %v", err)
	}
	jwtVerificationKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("Unable to parse ECDSA public key: %v", err)
	}
}

func main() {
	// Echo instance
	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(log.DEBUG)

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Initialize
	e.POST("/initialize", postInitialize)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)

	e.GET("/api/catalog/:jia_catalog_id", getCatalog)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/search", getIsuSearch)
	e.GET("/api/isu/:jia_isu_uuid", getIsu)
	e.PUT("/api/isu/:jia_isu_uuid", putIsu)
	e.DELETE("/api/isu/:jia_isu_uuid", deleteIsu)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.PUT("/api/isu/:jia_isu_uuid/icon", putIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/notification", getNotifications)
	e.GET("/api/notification/:jia_isu_uuid", getIsuNotifications)

	e.POST("/api/isu/:jia_isu_uuid/condition", postIsuCondition)

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("DB connection failed : %v", err)
		return
	}
	db.SetMaxOpenConns(10)
	defer db.Close()

	// Start server
	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_PORT", "3000"))
	e.Logger.Fatal(e.Start(serverPort))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := sessionStore.Get(r, sessionName)
	return session
}

func getUserIdFromSession(r *http.Request) (string, error) {
	session := getSession(r)
	userID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", fmt.Errorf("no session")
	}
	return userID.(string), nil
}

func getJIAServiceURL() string {
	config := Config{}
	err := db.Get(&config, "SELECT * FROM `isu_association_config` WHERE `name` = ?", "jia_service_url")
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultJIAServiceURL
	}
	if err != nil {
		log.Print(err)
		return DefaultJIAServiceURL
	}
	return config.URL
}

func postInitialize(c echo.Context) error {
	sqlDir := filepath.Join("..", "mysql", "db")
	paths := []string{
		filepath.Join(sqlDir, "0_Schema.sql"),
	}

	for _, p := range paths {
		sqlFile, _ := filepath.Abs(p)
		cmdStr := fmt.Sprintf("mysql -h %v -u %v -p%v -P %v %v < %v",
			mySQLConnectionData.Host,
			mySQLConnectionData.User,
			mySQLConnectionData.Password,
			mySQLConnectionData.Port,
			mySQLConnectionData.DBName,
			sqlFile,
		)
		err := exec.Command("bash", "-c", cmdStr).Run()
		if err != nil {
			c.Logger().Errorf("Initialize script error : %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "go",
	})
}

//  POST /api/auth
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	// verify JWT
	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(fmt.Sprintf("Unexpected signing method: %v", token.Header["alg"]), jwt.ValidationErrorSignatureInvalid)
		}
		return jwtVerificationKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Errorf("unknown error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	// get jia_user_id from JWT Payload
	claims, _ := token.Claims.(jwt.MapClaims) // TODO: 型アサーションのチェックの有無の議論
	jiaUserIdVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserId, ok := jiaUserIdVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	err = func() error { //TODO: 無名関数によるラップの議論
		tx, err := db.Beginx()
		if err != nil {
			return fmt.Errorf("failed to begin tx: %w", err)
		}
		defer tx.Rollback()

		var userNum int
		err = tx.Get(&userNum, "SELECT COUNT(*) FROM user WHERE `jia_user_id` = ? FOR UPDATE", jiaUserId)
		if err != nil {
			return fmt.Errorf("select user: %w", err)
		} else if userNum == 1 {
			// user already signup. only return cookie
			return nil
		}

		_, err = tx.Exec("INSERT INTO user (`jia_user_id`) VALUES (?)", jiaUserId)
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}

		err = tx.Commit()
		if err != nil {
			return fmt.Errorf("commit tx: %w", err)
		}
		return nil
	}()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session := getSession(c.Request())
	session.Values["jia_user_id"] = jiaUserId
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Errorf("failed to set cookie: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

//  POST /api/signout
func postSignout(c echo.Context) error {
	// ユーザからの入力
	// * session
	// session が存在しなければ 401

	// cookie の max-age を -1 にして Set-Cookie

	// response 200
	return fmt.Errorf("not implemented")
}

// TODO
// GET /api/user/{jia_user_id}
// ユーザ情報を取得
// day2 実装のため skip
// func getUser(c echo.Context) error {
// }

func getMe(c echo.Context) error {
	userID, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not signed in") // TODO 記法が決まったら修正
	}

	response := GetMeResponse{JIAUserID: userID}
	return c.JSON(http.StatusOK, response)
}

// GET /api/catalog/{jia_catalog_id}
// カタログ情報を取得
func getCatalog(c echo.Context) error {
	// ログインチェック
	_, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not signed in")
	}

	JIACatalogID := c.Param("jia_catalog_id")
	if JIACatalogID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "jia_catalog_id is missing")
	}

	// 日本ISU協会に問い合わせる
	catalogFromJIA, statusCode, err := getCatalogFromJIA(JIACatalogID)
	if err != nil {
		c.Logger().Errorf("failed to get catalog from JIA: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if statusCode == http.StatusNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "invalid jia_catalog_id")
	}

	catalog, err := castCatalogFromJIA(catalogFromJIA)
	if err != nil {
		c.Logger().Errorf("failed to cast catalog: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, catalog)
}

// 日本ISU協会にカタログ情報を問い合わせる
// 日本ISU協会のAPIについては資料を参照
func getCatalogFromJIA(catalogID string) (*CatalogFromJIA, int, error) {
	targetURLStr := getJIAServiceURL() + "/api/catalog/" + catalogID
	res, err := http.Get(targetURLStr)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode, nil
	}

	catalog := &CatalogFromJIA{}
	err = json.NewDecoder(res.Body).Decode(&catalog)
	if err != nil {
		return nil, http.StatusOK, err
	}
	return catalog, http.StatusOK, nil
}

//CatalogFromJIAからCatalogへのキャスト
func castCatalogFromJIA(catalogFromJIA *CatalogFromJIA) (*Catalog, error) {
	//TODO: バリデーション
	catalog := &Catalog{}
	catalog.JIACatalogID = catalogFromJIA.ID
	catalog.Name = catalogFromJIA.Name
	catalog.LimitWeight = catalogFromJIA.LimitWeight
	catalog.Weight = catalogFromJIA.Weight
	catalog.Size = catalogFromJIA.Size
	catalog.Maker = catalogFromJIA.Maker
	catalog.Tags = catalogFromJIA.Features
	return catalog, nil
}

func getIsuList(c echo.Context) error {
	jiaUserID, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not signed in")
	}

	limitStr := c.QueryParam("limit")
	limit := isuListLimit
	if limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid value: limit")
		}
	}

	isuList := []Isu{}
	err = db.Select(
		&isuList,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `is_deleted` = false ORDER BY `created_at` DESC LIMIT ?",
		jiaUserID, limit)
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	return c.JSON(http.StatusOK, isuList)
}

//  POST /api/isu
// 自分のISUの登録
func postIsu(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// input
	// 		jia_isu_uuid: 椅子固有のID（衝突しないようにUUID的なもの設定）
	// 		isu_name: 椅子の名前

	// req := contextからいい感じにinputとuser_idを取得
	// 形式が違うかったら400
	// (catalog,charactor), err := 外部API
	// ISU 協会にactivate
	// request
	// 	* jia_isu_uuid
	// response
	// 	* jia_catalog_id
	// 	* charactor
	// レスポンスが200以外なら横流し
	// 404, 403(認証拒否), 400, 5xx
	// 403はday2

	// imageはデフォルトを挿入
	// INSERT INTO isu VALUES (jia_isu_uuid, isu_name, image, catalog_, charactor, jia_user_id);
	// jia_isu_uuid 重複時 409

	// SELECT (*) FROM isu WHERE jia_user_id = `jia_user_id` and jia_isu_uuid = `jia_isu_uuid` and is_deleted=false;
	// 画像までSQLで取ってくるボトルネック
	// imageも最初はとってるけどレスポンスに含まれてないからselect時に持ってくる必要ない

	// response 200
	//{
	// * id
	// * name
	// * jia_catalog_id
	// * charactor
	//]
	return fmt.Errorf("not implemented")
}

// GET /api/isu/search
// 自分の ISU 一覧から検索
func getIsuSearch(c echo.Context) error {
	// input (query_param)
	//  * name
	//  * charactor
	//	* catalog_name
	//	* min_limit_weight
	//	* max_limit_weight
	//	* catalog_tags (ジェイウォーク)
	//  * page: （default = 1)
	//	* TODO: day2 二つのカラムを併せて計算した値での検索（x*yの面積での検索とか）

	jiaUserID, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not sign in")
	}
	//optional query param
	name := c.QueryParam("name")
	charactor := c.QueryParam("charactor")
	catalogName := c.QueryParam("catalog_name")
	minLimitWeightStr := c.QueryParam("min_limit_weight")
	maxLimitWeightStr := c.QueryParam("max_limit_weight")
	catalogTags := c.QueryParam("catalog_tags")
	pageStr := c.QueryParam("page")
	var minLimitWeight int
	var maxLimitWeight int
	var page int
	if minLimitWeightStr != "" {
		minLimitWeight, err = strconv.Atoi(minLimitWeightStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad format: min_limit_weight")
		}
	}
	if maxLimitWeightStr != "" {
		maxLimitWeight, err = strconv.Atoi(maxLimitWeightStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad format: max_limit_weight")
		}
	}
	if pageStr != "" {
		page, err = strconv.Atoi(pageStr)
		if err != nil || page <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "bad format: page")
		}
	}

	// DBからISU一覧を取得
	query := "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `is_deleted` = false"
	queryParam := []interface{}{jiaUserID}
	if name != "" {
		query += " `name` LIKE ? "
		queryParam = append(queryParam, name)
	}
	if charactor != "" {
		query += " `charactor` = ? "
		queryParam = append(queryParam, charactor)
	}
	// query += " LIMIT ? "
	// queryParam = append(queryParam, searchLimit)
	// if pageStr != "" {
	// 	query += " OFFSET ? "
	// 	queryParam = append(queryParam, page)
	// }
	query += " ORDER BY `created_at` DESC "
	isuList := []*Isu{}
	err = db.Select(&isuList,
		query,
		queryParam...,
	)

	isuListResponse := []*Isu{}
	for _, isu := range isuList {
		// ISU協会に問い合わせ(http request)
		catalogFromJIA, statusCode, err := getCatalogFromJIA(isu.JIACatalogID)
		if err != nil {
			c.Logger().Errorf("failed to get catalog from JIA: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError)
		}
		if statusCode == http.StatusNotFound {
			return echo.NewHTTPError(http.StatusNotFound, "invalid jia_catalog_id")
		}

		catalog, err := castCatalogFromJIA(catalogFromJIA)
		if err != nil {
			c.Logger().Errorf("failed to cast catalog: %v", err)
			return echo.NewHTTPError(http.StatusInternalServerError)
		}

		// isu_calatog情報を使ってフィルター
		//if !(min_limit_weight < catalog.limit_weight && catalog.limit_weight < max_limit_weight) {
		//	continue
		//}

		// ... catalog_name,catalog_tags

		// res の 配列 append
	}
	// 指定pageからlimit件数（固定）だけ取り出す(ボトルネックにしたいのでbreakは行わない）

	if searchLimit < len(isuListResponse) {
		isuListResponse = isuListResponse[:searchLimit]
	}
	return c.JSON(http.StatusOK, isuListResponse)
}

//  GET /api/isu/{jia_isu_uuid}
// 椅子の情報を取得する
func getIsu(c echo.Context) error {
	jiaUserID, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not sign in")
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	// TODO: jia_user_id 判別はクエリに入れずその後のロジックとする？ (一通り完成した後に要考慮)
	var isu Isu
	err = db.Get(&isu, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ? AND `is_deleted` = ?",
		jiaUserID, jiaIsuUUID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "isu not found")
	}
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	return c.JSON(http.StatusOK, isu)
}

//  PUT /api/isu/{jia_isu_uuid}
// 自分の所有しているISUの情報を変更する
func putIsu(c echo.Context) error {
	jiaUserID, err := getUserIdFromSession(c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "you are not sign in")
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	var req PutIsuRequest
	err = c.Bind(&req)
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request")
	}

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	defer tx.Rollback()

	var count int
	err = tx.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ? AND `is_deleted` = ?",
		jiaUserID, jiaIsuUUID, false)
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if count == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "isu not found")
	}

	_, err = tx.Exec("UPDATE `isu` SET `name` = ? WHERE `jia_isu_uuid` = ?", req.Name, jiaIsuUUID)
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	var isu Isu
	err = tx.Get(&isu, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ? AND `is_deleted` = ?",
		jiaUserID, jiaIsuUUID, false)
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Error(err)
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	return c.JSON(http.StatusOK, isu)
}

//  DELETE /api/isu/{jia_isu_uuid}
// 所有しているISUを削除する
func deleteIsu(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// input
	// 		* jia_isu_uuid: 椅子の固有ID

	// トランザクション開始

	// DBから当該のISUが存在するか検索
	// SELECT (*) FROM isu WHERE jia_user_id = `jia_user_id` and jia_isu_uuid = `jia_isu_uuid` and is_deleted=false;
	// 存在しない場合 404 を返す

	// 存在する場合 ISU　の削除フラグを有効にして 204 を返す
	// UPDATE isu SET is_deleted = true WHERE jia_isu_uuid = `jia_isu_uuid`;

	// ISU協会にdectivateを送る
	// MEMO: ISU協会へのリクエストが失敗した時に DB をロールバックできるから

	// トランザクション終了
	// MEMO: もしコミット時にエラーが発生しうるならば、「ISU協会側はdeactivate済みだがDBはactive」という不整合が発生しうる

	//response 204
	return fmt.Errorf("not implemented")
}

//  GET /api/isu/{jia_isu_uuid}/icon
// ISUのアイコンを取得する
// MEMO: ヘッダーとかでキャッシュ効くようにするのが想定解？(ただしPUTはあることに注意)
//       nginxで認証だけ外部に投げるみたいなのもできるっぽい？（ちゃんと読んでいない）
//       https://tech.jxpress.net/entry/2018/08/23/104123
// MEMO: DB 内の image は longblob
func getIsuIcon(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// SELECT image FROM isu WHERE jia_user_id = `jia_user_id` and jia_isu_uuid = `jia_isu_uuid` and is_deleted=false;
	// 見つからなければ404
	// user_idがリクエストユーザーのものでなければ404

	// response 200
	// image
	// MEMO: とりあえず未指定... Content-Type: image/png image/jpg
	return fmt.Errorf("not implemented")
}

//  PUT /api/isu/{jia_isu_uuid}/icon
// ISUのアイコンを登録する
// multipart/form-data
// MEMO: DB 内の image は longblob
func putIsuIcon(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	//トランザクション開始

	// SELECT image FROM isu WHERE jia_user_id = `jia_user_id` and jia_isu_uuid = `jia_isu_uuid` and is_deleted=false;
	// 見つからなければ404
	// user_idがリクエストユーザーのものでなければ404

	// UPDATE isu SET image=? WHERE jia_user_id = `jia_user_id` and jia_isu_uuid = `jia_isu_uuid` and is_deleted=false;

	//トランザクション終了

	// response 200
	// {}
	return fmt.Errorf("not implemented")
}

//  GET /api/isu/{jia_isu_uuid}/graph
// グラフ描画のための情報を計算して返却する
// ユーザーがISUの機嫌を知りたい
// この時間帯とか、この日とかの機嫌を知りたい
// 日毎時間単位グラフ
// conditionを何件か集めて、ISUにとっての快適度数みたいな値を算出する
func getIsuGraph(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// input (path_param)
	//	* jia_isu_uuid: 椅子の固有ID
	// input (query_param)
	//	* date (required)
	//		YYYY-MM-DD
	//

	// 自分のISUかチェック
	// SELECT count(*) from isu where jia_user_id=`jia_user_id` and id = `jia_isu_uuid` and is_deleted=false;
	// エラー: response 404

	// MEMO: シナリオ的にPostIsuConditionでgraphを生成する方がボトルネックになる想定なので初期実装はgraphテーブル作る
	// DBを検索。グラフ描画に必要な情報を取得
	// ボトルネック用に事前計算したものを取得
	// graphは POST /api/isu/{jia_isu_uuid}/condition で生成
	// SELECT * from graph
	//   WHERE jia_isu_uuid = `jia_isu_uuid` AND date<=start_at AND start_at < (date+1day)

	//SQLのレスポンスを成形
	//nullチェック

	// response 200:
	//    グラフ描画のための情報のjson
	//        * フロントで表示しやすい形式
	// [{
	// start_at: ...
	// end_at: ...
	// data: {
	//   score: 70,//>=0
	//   sitting: 50 (%),
	//   detail: {
	//     dirty: -10,
	//     over_weight: -20
	//   }
	// }},
	// {
	// start_at: …,
	// end_at: …,
	// data: null,
	// },
	// {...}, ...]
	return fmt.Errorf("not implemented")
}

//  GET /api/notification?
// 自分の所持椅子の通知を取得する
// MEMO: 1970/1/1みたいな時を超えた古代からのリクエストは表示するか → する
// 順序は最新順固定
func getNotifications(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// input {jia_isu_uuid}が無い以外は、/api/notification/{jia_isu_uuid}と同じ
	//

	// cookieからユーザID取得
	// ユーザの所持椅子取得
	// SELECT * FROM isu where jia_user_id = ?;

	// ユーザの所持椅子毎に /api/notificaiton/{jia_isu_uuid} を叩く（こことマージ含めてボトルネック）
	// query_param は GET /api/notification (ここ) のリクエストと同じものを使い回す

	// ユーザの所持椅子ごとのデータをマージ（ここと個別取得部分含めてボトルネック）
	// 通知時間帯でソートして、limit件数（固定）該当するデータを返す
	// MEMO: 改善後はこんな感じのSQLで一発でとる
	// select * from isu_log where (isu_log.created_at, jia_isu_uuid) < (cursor.end_time, cursor.jia_isu_uuid)
	//  order by created_at desc,jia_isu_uuid desc limit ?
	// 10.1.36-MariaDB で確認

	//memo（没）
	// (select * from isu_log where (isu_log.created_at=cursor.end_time and jia_isu_uuid < cursor.jia_isu_uuid)
	//   or isu_log.created_at<cursor.end_time order by created_at desc,jia_isu_uuid desc limit ?)

	// response: 200
	// /api/notification/{jia_isu_uuid}と同じ
	return fmt.Errorf("not implemented")
}

//  GET /api/notification/{jia_isu_uuid}?start_time=
// 自分の所持椅子のうち、指定したisu_idの通知を取得する
func getIsuNotifications(c echo.Context) error {
	// * session
	// session が存在しなければ 401

	// input
	//     * jia_isu_uuid: 椅子の固有番号(path_param)
	//     * start_time: 開始時間
	//     * cursor_end_time: 終了時間 (required)
	//     * cursor_isu_id (required)
	//     * condition_level: critical,warning,info (csv)
	//               critical: conditions (is_dirty,is_overweight,is_broken) のうちtrueが3個
	//               warning: conditionsのうちtrueのものが1 or 2個
	//               info: warning無し
	//     * MEMO day2実装: message (文字列検索)

	// memo
	// 例: Google Cloud Logging の URL https://console.cloud.google.com/logs/query;query=resource.type%3D%22gce_instance%22%20resource.labels.instance_id%3D%22<DB_NAME>%22?authuser=1&project=<PROJECT_ID>&query=%0A

	// cookieからユーザID取得

	// isu_id存在確認、ユーザの所持椅子か確認
	// 対象isu_idのisu_name取得
	// 存在しなければ404

	// 対象isu_idの通知を取得(limit, cursorで絞り込み）
	// select * from isu_log where jia_isu_uuid = {jia_isu_uuid} AND (isu_log.created_a, jia_isu_uuid) < (cursor.end_time, cursor.jia_isu_uuid) order by created_at desc, jia_isu_uuid desc limit ?
	// MEMO: ↑で実装する

	//for {
	// conditions を元に condition_level (critical,warning,info) を算出
	//}

	// response: 200
	// [{
	//     * jia_isu_uuid
	//     * isu_name
	//     * timestamp
	//     * conditions: {"is_dirty": boolean, "is_overweight": boolean,"is_broken": boolean}
	//     * condition_level
	//     * message
	// },...]
	return fmt.Errorf("not implemented")
}

// POST /api/isu/{jia_isu_uuid}/condition
// ISUからのセンサデータを受け取る
// MEMO: 初期実装では認証をしない（isu_id 知ってるなら大丈夫だろ〜）
// MEMO: Day2 ISU協会からJWT発行や秘密鍵公開鍵ペアでDBに公開鍵を入れる？
// MEMO: ログが一件増えるくらいはどっちでもいいのでミスリーディングにならないようトランザクションはとらない方針
// MEMO: 1970/1/1みたいな時を超えた古代からのリクエストがきた場合に、ここの時点で弾いてしまうのか、受け入れるか → 受け入れる (ただし jia_isu_uuid と timpestamp の primary 制約に違反するリクエストが来たら 409 を返す)
// MEMO: DB Schema における conditions の型は取り敢えず string で、困ったら考える
//     (conditionカラム: is_dirty=true,is_overweight=false)
func postIsuCondition(c echo.Context) error {
	// input (path_param)
	//	* jia_isu_uuid
	// input (body)
	//  * is_sitting:  true/false,
	// 	* condition: {
	//      is_dirty:    true/false,
	//      is_overweight: true/false,
	//      is_broken:   true/false,
	//    }
	//  * message
	//	* timestamp（秒まで）
	// invalid ならば 400

	//  memo (実装しないやつ)
	// 	* condition: {
	//      sitting: {message: "hoge"},
	//      dirty:   {message: "ほこりがたまってる"},
	//      over_weight:  {message: "右足が辛い"}
	//    }

	// トランザクション開始

	// DBから jia_isu_uuid が存在するかを確認
	// 		SELECT id from isu where id = `jia_isu_uuid` and is_deleted=false
	// 存在しない場合 404 を返す

	//  memo → getNotifications にて実装
	// conditionをもとにlevelを計算する（info, warning, critical)
	// info 悪いコンディション数が0
	// warning 悪いコンディション数がN個以上
	// critical 悪いコンディション数がM個以上

	// 受け取った値をDBにINSERT  // conditionはtextカラム（初期実装）
	// 		INSERT INTO isu_log VALUES(jia_isu_uuid, message, timestamp, conditions );
	// もし primary 制約により insert が失敗したら 409

	// getGraph用のデータを計算
	// 初期実装では該当するisu_idのものを全レンジ再計算
	// SELECT * from isu_log where jia_isu_uuid = `jia_isu_uuid`;
	// ↑ and timestamp-1h<timestamp and timestamp < timestamp+1hをつけることも検討
	// isu_logを一時間ごとに区切るfor {
	//  if (区切り) {
	//    sumを取って減点を生成(ただし0以上にする)
	//    割合からsittingを生成
	//    ここもう少し重くしたい
	// https://dev.mysql.com/doc/refman/5.6/ja/insert-on-duplicate.html
	// dataはJSON型
	//    INSERT INTO graph VALUES(jia_isu_uuid, time_start, time_end, data) ON DUPLICATE KEY UPDATE;
	// data: {
	//   score: 70,//>=0
	//   sitting: 50 (%),
	//   detail: {
	//     dirty: -10,
	//     over_weight: -20
	//   }
	// }
	//vvvvvvvvvv memoここから vvvvvvvvvv
	// 一時間ごとに集積した着席時間
	//   condition における総件数からの、座っている/いないによる割合
	// conditionを何件か集めて、ISUにとっての快適度数みたいな値を算出する
	//   減点方式 conditionの種類ごとの点数*件数
	//^^^^^^^^^^^^^^^ memoここまで ^^^^^^^^^^^

	// トランザクション終了

	// response 201
	return fmt.Errorf("not implemented")
}
