package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bhmj/jsonslice"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
	"os"
	"park/config"
	"strconv"
	"time"
)

var ipRequestCheckCompany = make(map[string]map[string]int)

var API_URL_CARD = "https://zachestnyibiznesapi.ru/paid/data/card"
var API_URL_FSSP = "https://zachestnyibiznesapi.ru/paid/data/fssp-list"
var API_URL_FNS = "https://zachestnyibiznesapi.ru/paid/data/fns-card"

func GetCompany(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")

	if accessToken == "" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var token config.Token
	errToken := db.Where(config.Token{AccessToken: accessToken}).First(&token)
	if errToken.Error != nil || errToken.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	} else if token.ValidTrough.Time.Before(time.Now()) {
		db.Delete(&token, token.ID)
		return c.JSON(http.StatusForbidden, nil)
	}

	var user config.User
	errUser := db.Where(config.User{ID: token.UserID}).First(&user)
	if errUser.Error != nil || errUser.RowsAffected == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}


	var company config.Company
	errCompany := db.Where(config.Company{INN: user.CompanyINN}).First(&company)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}

	return c.JSON(http.StatusOK, company)
}

func ListCompanies(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")

	userRole := CheckUserRole(accessToken)

	if userRole != "admin" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var companies []config.Company
	errCompany := db.Find(&companies)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}
	return c.JSON(http.StatusOK, companies)

}

func CheckCompany(c echo.Context) error {
	ip := c.RealIP()
	today := time.Now().Format("2006-01-02")

	if ipRequestCheckCompany[ip] == nil {
		ipRequestCheckCompany[ip] = make(map[string]int)
	}
	if ipRequestCheckCompany[ip][today] > 10 {
		return c.JSON(http.StatusTooManyRequests, "Превышен лимит запросов на сегодня с этого IP")
	}
	ipRequestCheckCompany[ip][today]++

	db := config.DB()

	inn := c.Request().Header.Get("inn")
	if inn == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	cardDataJson := ZCBSendToQueue(func() []byte {
		return zcbRequest(inn, API_URL_CARD)
	})

	ogrn := checkJsonCompany(cardDataJson, "$.body.docs.0.ОГРН")
	if ogrn == "" {
		ogrn = checkJsonCompany(cardDataJson, "$.body.docs.0.ОГРНИП")
		if ogrn == "" {
			return c.JSON(http.StatusForbidden, "Такой компании не существует")
		}
	}


	fsspDataJson := ZCBSendToQueue(func() []byte {
		return zcbRequest(ogrn, API_URL_FSSP)
	})

	fnsDataJson := ZCBSendToQueue(func() []byte {
		return zcbRequest(ogrn, API_URL_FNS)
	})

	var rejects []string

	if checkJsonCompany(cardDataJson, "$.body.docs.0.ТипДокумента") != "ul" &&
		checkJsonCompany(cardDataJson, "$.body.docs.0.ТипДокумента") != "ip" {
		rejects = append(rejects, "Заявитель является физическим лицом.")
	}

	if checkJsonCompany(cardDataJson, "$.body.docs.0.Реестр01") != "0" &&
		checkJsonCompany(cardDataJson, "$.body.docs.0.Реестр02") != "0" {
		rejects = append(rejects, "Юридическое лицо имеет взыскиваемую судебными приставами задолженность по уплате налогов, превышающую 1000 рублей или юридическое лицо не представляет налоговую отчетность более года.")
	}

	if checkJsonCompany(cardDataJson, "$.body.docs.0.ТипДокумента") == "ul" &&
		checkJsonCompany(cardDataJson, "$.body.docs.0.КатСубМСП.1") != "Микро" &&
		checkJsonCompany(cardDataJson, "$.body.docs.0.КатСубМСП.1") != "Малое" &&
		checkJsonCompany(cardDataJson, "$.body.docs.0.КатСубМСП.1") != "Среднее" {
		rejects = append(rejects, "Компания заявителя не находится в реестре МСП.")
	}

	var blockedOkved config.BlockedOkveds
	mainOkved := checkJsonCompany(cardDataJson, "$.body.docs.0.КодОКВЭД")

	errOkved := db.Where(config.BlockedOkveds{Code: mainOkved}).First(&blockedOkved)
	if errOkved.RowsAffected != 0 {
		rejects = append(rejects, "Код деятельности компании попадает под ряд запрещенных к субсидированию. "+blockedOkved.Code+": "+blockedOkved.Name)
	}

	if checkJsonCompany(cardDataJson, "$.body.docs.0.СвОКВЭДДоп") != "ul" {
		var minorOkvedslocal []minorOkveds
		err := json.Unmarshal([]byte("["+checkJsonCompany(cardDataJson, "$.body.docs.0.СвОКВЭДДоп")+"]"), &minorOkvedslocal)
		if err != nil {
			panic(err)
		}
		for index := range minorOkvedslocal {
			tempOkved := minorOkvedslocal[index]
			errOkved = db.Where(config.BlockedOkveds{Code: tempOkved.Code}).First(&blockedOkved)
			if errOkved.RowsAffected != 0 {
				rejects = append(rejects, "Код деятельности компании попадает под ряд запрещенных к субсидированию. "+tempOkved.Code+": "+tempOkved.Name)
			}
		}
	}

	if checkJsonCompany(cardDataJson, "$.body.docs.0.Активность") != "Действующее" {
		rejects = append(rejects, "Компания ликвидирована или находится в процессе ликвидации, либо наложены другие ограничения на ведение деятельности.")
	}

	debtArray, _ := convertToJSON("[" + checkJsonCompany(fsspDataJson, "$.body.docs") + "]")
	for i := range debtArray {
		debt := checkJsonCompany(fsspDataJson, "$.body.docs."+strconv.Itoa(i)+".ОстатокДолга")
		if debt != "0" {
			rejects = append(rejects, "Имеется непогашенный долг (по данным ФССП): "+debt)
		}
	}

	if checkJsonCompany(fnsDataJson, "$.body.СвРеорг.СвСтатус.@attributes.КодСтатусЮЛ") == "132" ||
		checkJsonCompany(fnsDataJson, "$.body.СвСтатус.СвСтатус.@attributes.КодСтатусЮЛ") == "132" {
		rejects = append(rejects, "Компания находится в процессе ликвидации.")
	}

	if len(rejects) != 0 {
		return c.JSON(http.StatusLocked, rejects)
	}

	company := config.Company{
		INN:      inn,
		OGRN:     ogrn,
		FsspData: fsspDataJson,
		CardData: cardDataJson,
		FnsData:  fnsDataJson,
	}

	res := db.Where(config.Company{INN: inn})
	if res.RowsAffected != 0 {
		res.Updates(company)
		return c.JSON(http.StatusOK, nil)
	}

	result := db.Create(&company)
	if result.Error != nil {
		return c.JSON(http.StatusOK, nil)
	}

	return c.JSON(http.StatusOK, nil)
}

func checkJsonCompany(json []byte, path string) string {
	res, _ := jsonslice.Get(json, path)

	if len(res) < 3 {
		return string(res)
	}
	return string(res)[1 : len(res)-1]
}

func convertToJSON(s string) ([]map[string]interface{}, error) {
	var jsonArray []map[string]interface{}
	err := json.Unmarshal([]byte(s), &jsonArray)
	if err == nil {
		return jsonArray, nil
	}

	var jsonObject map[string]interface{}
	err = json.Unmarshal([]byte(s), &jsonObject)
	if err == nil {
		return []map[string]interface{}{jsonObject}, nil
	}
	return nil, errors.New("invalid JSON format")
}

func zcbRequest(inn string, url string) []byte {
	apiKey, _ := os.LookupEnv("ZCB_API_KEY")
	requestURL := fmt.Sprintf("%s?id=%s&api_key=%s", url, inn, apiKey)
	resp, err := http.Get(requestURL)
	if err != nil {
		println(err.Error())
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		println(err.Error())
		return nil
	}
	var dataJson json.RawMessage
	err = json.Unmarshal(body, &dataJson)
	if err != nil {
		println(err.Error())
		return nil
	}
	return dataJson
}

type minorOkveds struct {
	Code string `json:"КодОКВЭД"`
	Name string `json:"НаимОКВЭД"`
}
