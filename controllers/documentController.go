package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bhmj/jsonslice"
	"github.com/labstack/echo/v4"
	minio2 "github.com/minio/minio-go/v7"
	"net/http"
	"os"
	"park/config"
	"strconv"
	"time"
)

var ipRequestFindAnon = make(map[string]map[string]int)

func CreateDecree(c echo.Context) error {
	db := config.DB()
	minio := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	newDecree := c.Request().Header.Get("newDecree")

	if newDecree == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var decree config.Decree

	if err := json.Unmarshal([]byte(newDecree), &decree); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	if err := db.Create(&decree).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	file, err := c.FormFile("file")
	if err != nil {
		println(err.Error())
		return c.String(http.StatusBadRequest, "Failed to get file")
	}

	// Открываем файл из запроса
	src, err := file.Open()
	if err != nil {
		return c.String(http.StatusInternalServerError, fmt.Sprintf("Failed to open uploaded file: %v", err))
	}
	defer src.Close()

	// Генерируем имя объекта и определяем тип контента
	objectName := "decree/" + strconv.Itoa(decree.ID) + "/" + file.Filename
	contentType := file.Header.Get("Content-Type")

	bucket, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	// Загружаем файл в MinIO
	_, err = minio.PutObject(
		context.Background(),
		bucket,
		objectName,
		src, // Используем поток файла напрямую
		file.Size,
		minio2.PutObjectOptions{ContentType: contentType},
	)
	if err != nil {
		return c.String(http.StatusInternalServerError, fmt.Sprintf("Failed to upload to MinIO: %v", err))
	}

	decree.FileName = file.Filename
	errDecree := db.Save(&decree).Error
	if errDecree != nil {
		return c.JSON(http.StatusForbidden, nil)
	}

	decreeIdStr := strconv.Itoa(decree.ID)
	AddLog(user.ID, "Create Decree", decreeIdStr)

	return c.JSON(http.StatusOK, nil)
}

func ListDecree(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")

	userRole := CheckUserRole(accessToken)
	if userRole != "admin" && userRole != "moderator" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var decrees []config.Decree
	errDecrees := db.Where(config.Decree{}).Find(&decrees).Error
	if errDecrees != nil {
		return c.JSON(http.StatusForbidden, nil)
	}
	return c.JSON(http.StatusOK, decrees)
}

func DownloadDecree(c echo.Context) error {
	db := config.DB()
	minio := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	decreeID := c.Request().Header.Get("decreeID")

	if decreeID == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	userRole := CheckUserRole(accessToken)
	if userRole != "admin" && userRole != "moderator" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	decreeIDINT, _ := strconv.Atoi(decreeID)

	var decree config.Decree

	errDecree := db.Where(config.Decree{ID: decreeIDINT}).First(&decree)
	if errDecree.Error != nil || errDecree.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}

	bucket, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	object, err := minio.GetObject(c.Request().Context(), bucket, "decree/"+decreeID+"/"+decree.FileName, minio2.GetObjectOptions{})
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to get object from MinIO: "+err.Error())
	}

	stat, err := object.Stat()
	if err != nil {
		return c.String(http.StatusInternalServerError, "Failed to get object metadata: "+err.Error())
	}

	c.Response().Header().Set("Content-Disposition", "attachment; filename="+decree.FileName)
	c.Response().Header().Set("Content-Type", stat.ContentType)
	c.Response().Header().Set("Content-Length", string(stat.Size))

	return c.Stream(http.StatusOK, stat.ContentType, object)

}

func EditDecree(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")
	decreeId := c.Request().Header.Get("decreeId")
	editedDecree := c.Request().Header.Get("editedDecree")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var decree config.Decree
	if err := db.First(&decree, decreeId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	var updatedDecree config.Decree
	if err := json.Unmarshal([]byte(editedDecree), &updatedDecree); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	updatedDecree.ID = decree.ID
	updatedDecree.FileName = decree.FileName

	decree = updatedDecree

	if err := db.Save(&decree).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Edit Decree", decreeId)

	return c.JSON(http.StatusOK, nil)
}

func DeleteDecree(c echo.Context) error {
	db := config.DB()
	minio := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	decreeId := c.Request().Header.Get("decreeId")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var decree config.Decree
	if err := db.First(&decree, decreeId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	// Удаляем файл из MinIO
	bucket, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	prefix := "decree/" + strconv.Itoa(decree.ID) + "/"

	objectCh := minio.ListObjects(c.Request().Context(), bucket, minio2.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for object := range objectCh {
		if object.Err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Error listing objects: %v", object.Err))
		}

		err := minio.RemoveObject(c.Request().Context(), bucket, object.Key, minio2.RemoveObjectOptions{})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Failed to delete object %s: %v", object.Key, err))
		}
	}

	// Удаляем запись из базы данных
	if err := db.Delete(&decree).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Delete Decree", decreeId)

	return c.JSON(http.StatusOK, nil)
}

func CreateGrant(c echo.Context) error {
	db := config.DB()
	minio := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	newGrant := c.Request().Header.Get("newGrant")

	if newGrant == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var grant config.Grant

	if err := json.Unmarshal([]byte(newGrant), &grant); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	if err := db.Create(&grant).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	// Получаем список файлов из формы
	form, err := c.MultipartForm()
	if err != nil {
		return c.JSON(http.StatusBadRequest, "Failed to parse multipart form")
	}

	files := form.File["files"] // `files` — ключ массива файлов в запросе
	if len(files) == 0 {
		return c.JSON(http.StatusBadRequest, "No files uploaded")
	}

	bucket, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	var fileNames []string

	for _, file := range files {
		// Открываем каждый файл
		src, err := file.Open()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Failed to open uploaded file: %v", err))
		}
		defer src.Close()

		// Генерируем имя объекта
		objectName := "grant/" + strconv.Itoa(grant.ID) + "/" + file.Filename
		contentType := file.Header.Get("Content-Type")

		// Загружаем файл в MinIO
		_, err = minio.PutObject(
			context.Background(),
			bucket,
			objectName,
			src, // Используем поток файла напрямую
			file.Size,
			minio2.PutObjectOptions{ContentType: contentType},
		)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Failed to upload file %s to MinIO: %v", file.Filename, err))
		}

		fileNames = append(fileNames, file.Filename)
	}

	fileNamesJSON, err := json.Marshal(fileNames)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Failed to marshal file names: %v", err))
	}

	grant.FileNames = fileNamesJSON

	errGrant := db.Save(&grant).Error
	if errGrant != nil {
		return c.JSON(http.StatusForbidden, nil)
	}

	AddLog(user.ID, "Create grant", strconv.Itoa(grant.ID))

	return c.JSON(http.StatusOK, nil)
}

func ListGrant(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")

	userRole := CheckUserRole(accessToken)
	if userRole != "admin" && userRole != "moderator" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var grants []config.Grant
	errGrants := db.Where(config.Grant{}).Find(&grants).Error
	if errGrants != nil {
		return c.JSON(http.StatusForbidden, nil)
	}
	return c.JSON(http.StatusOK, grants)
}

func FindGrant(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")

	user := getUserObject(accessToken)

	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	if user.Role == "admin" || user.Role == "tester" {
		var decrees []config.Decree
		if err := db.Order("id desc").Limit(50).Find(&decrees).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		var grants []config.Grant
		if err := db.Order("id desc").Limit(50).Find(&grants).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		var samples []config.Sample
		if err := db.Order("id desc").Limit(50).Find(&samples).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		return c.JSON(http.StatusOK, map[string]interface{}{
			"decrees": decrees,
			"grants":  grants,
			"samples": samples,
		})
	} else if user.Role == "moderator" {
		return c.JSON(http.StatusOK, "{\"decrees\": [], \"grants\": [], \"samples\": []}")
	}

	var company config.Company
	errCompany := db.Where(config.Company{INN: user.CompanyINN}).First(&company)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}

	tempList, _ := jsonslice.Get(company.CardData, "$.body.docs.0.СвОКВЭДДоп")
	mainOkved, _ := jsonslice.Get(company.CardData, "$.body.docs.0.КодОКВЭД")
	companyCity, _ := jsonslice.Get(company.CardData, "$.body.docs.0.НаимГород")
	if string(companyCity) == "null" {
		companyCity, _ = jsonslice.Get(company.CardData, "$.body.docs.0.НаимРайон")
	}
	companyRegion, _ := jsonslice.Get(company.CardData, "$.body.docs.0.НаимРегион")

	var parsedCity, parsedRegion string
	json.Unmarshal([]byte(companyCity), &parsedCity)
	json.Unmarshal([]byte(companyRegion), &parsedRegion)

	var items []map[string]interface{}
	err := json.Unmarshal([]byte(tempList), &items)
	if err != nil {
		return c.JSON(http.StatusForbidden, err)
	}

	codes := []string{}
	for _, item := range items {
		if code, ok := item["КодОКВЭД"].(string); ok {
			codes = append(codes, code)
		}
	}
	codes = append(codes, string(mainOkved)[1:len(mainOkved)-1])

	var decrees []config.Decree
	query := db.Model(&config.Decree{})

	for _, code := range codes {
		// Преобразуем JSONB в текст и ищем через LIKE
		query = query.Or("(CAST(okved_list AS TEXT) LIKE ? AND (LOWER(COALESCE(city, '')) = LOWER(?) OR (COALESCE(city, '') = '' AND LOWER(region) = LOWER(?))) )", "%"+code+"%", parsedCity, parsedRegion)
	}

	errDecrees := query.Find(&decrees).Error
	if errDecrees != nil {
		return c.JSON(http.StatusInternalServerError, errDecrees)
	}

	var grants []config.Grant

	for i := range decrees {
		var tmpGrants []config.Grant
		err := db.Where(config.Grant{DecreeID: decrees[i].ID}).Find(&tmpGrants).Error
		if err != nil {
			continue
		}
		grants = append(grants, tmpGrants...)
	}

	var samples []config.Sample

	for i := range grants {
		var tmpSamples []config.Sample
		err := db.Where(config.Sample{GrantID: grants[i].ID}).Find(&tmpSamples).Error
		if err != nil {
			continue
		}
		samples = append(samples, tmpSamples...)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"decrees": decrees,
		"grants":  grants,
		"samples": samples,
	})

}

func FindRegionalGrant(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")
	user := getUserObject(accessToken)
	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, "No user found")
	}

	if user.Role == "admin" || user.Role == "tester" {
		var decrees []config.Decree
		if err := db.Order("id desc").Limit(50).Find(&decrees).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		var grants []config.Grant
		if err := db.Order("id desc").Limit(50).Find(&grants).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		var samples []config.Sample
		if err := db.Order("id desc").Limit(50).Find(&samples).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, err)
		}

		return c.JSON(http.StatusOK, map[string]interface{}{
			"decrees": decrees,
			"grants":  grants,
			"samples": samples,
		})
	} else if user.Role == "moderator" {
		return c.JSON(http.StatusOK, "{\"decrees\": [], \"grants\": [], \"samples\": []}")
	}

	var company config.Company
	errCompany := db.Where(config.Company{INN: user.CompanyINN}).First(&company)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}

	companyRegion, _ := jsonslice.Get(company.CardData, "$.body.docs.0.НаимРегион")
	var parsedRegion string
	json.Unmarshal([]byte(companyRegion), &parsedRegion)

	var decrees []config.Decree
	query := db.Model(&config.Decree{})

	// Преобразуем JSONB в текст и ищем через LIKE
	query = query.Or("(LOWER(region) = LOWER(?))", parsedRegion)

	errDecrees := query.Find(&decrees).Error
	if errDecrees != nil {
		return c.JSON(http.StatusInternalServerError, errDecrees)
	}

	var grants []config.Grant

	for i := range decrees {
		var tmpGrants []config.Grant
		err := db.Where(config.Grant{DecreeID: decrees[i].ID}).Find(&tmpGrants).Error
		if err != nil {
			continue
		}
		grants = append(grants, tmpGrants...)
	}

	var samples []config.Sample

	for i := range grants {
		var tmpSamples []config.Sample
		err := db.Where(config.Sample{GrantID: grants[i].ID}).Find(&tmpSamples).Error
		if err != nil {
			continue
		}
		samples = append(samples, tmpSamples...)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"decrees": decrees,
		"grants":  grants,
		"samples": samples,
	})
}

func FindGrantAnon(c echo.Context) error {

	ip := c.RealIP()
	today := time.Now().Format("2006-01-02")

	if ipRequestFindAnon[ip] == nil {
		ipRequestFindAnon[ip] = make(map[string]int)
	}
	if ipRequestFindAnon[ip][today] > 10 {
		return c.JSON(http.StatusTooManyRequests, "Превышен лимит запросов на сегодня с этого IP")
	}
	ipRequestFindAnon[ip][today]++

	db := config.DB()
	companyINN := c.Request().Header.Get("companyINN")

	if companyINN == "" {
		return c.JSON(http.StatusUnauthorized, "No companyINN found")
	}

	var company config.Company
	errCompany := db.Where(config.Company{INN: companyINN}).First(&company)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, "no company found")
	}

	if company.ID == 0 {
		return c.JSON(http.StatusUnauthorized, "No company found")
	}

	companyRegion, _ := jsonslice.Get(company.CardData, "$.body.docs.0.НаимРегион")
	var parsedRegion string
	json.Unmarshal([]byte(companyRegion), &parsedRegion)

	var decrees []config.Decree
	query := db.Model(&config.Decree{})

	// Преобразуем JSONB в текст и ищем через LIKE
	query = query.Or("(LOWER(region) = LOWER(?))", parsedRegion)

	errDecrees := query.Find(&decrees).Limit(6).Error
	if errDecrees != nil {
		return c.JSON(http.StatusInternalServerError, errDecrees)
	}

	var grants []config.Grant

	for i := range decrees {
		var tmpGrants []config.Grant
		err := db.Where(config.Grant{DecreeID: decrees[i].ID}).Find(&tmpGrants).Limit(6).Error
		if err != nil {
			continue
		}
		grants = append(grants, tmpGrants...)
	}

	var samples []config.Sample

	for i := range grants {
		var tmpSamples []config.Sample
		err := db.Where(config.Sample{GrantID: grants[i].ID}).Find(&tmpSamples).Limit(6).Error
		if err != nil {
			continue
		}
		samples = append(samples, tmpSamples...)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"decrees": decrees,
		"grants":  grants,
		"samples": samples,
	})
}

func EditGrant(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")
	grantId := c.Request().Header.Get("grantId")
	editedGrant := c.Request().Header.Get("editedGrant")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	if editedGrant == "" {
		return c.JSON(http.StatusBadRequest, "No edited grant provided")
	}

	var grant config.Grant
	if err := db.First(&grant, grantId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	var updatedGrant config.Grant
	if err := json.Unmarshal([]byte(editedGrant), &updatedGrant); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	var existingDocuments map[string]bool
	if err := json.Unmarshal(grant.Documents, &existingDocuments); err != nil {
		return c.JSON(http.StatusInternalServerError, "Failed to unmarshal existing Documents")
	}

	var updatedDocuments map[string]bool
	if err := json.Unmarshal(updatedGrant.Documents, &updatedDocuments); err != nil {
		return c.JSON(http.StatusBadRequest, "Failed to unmarshal updated Documents")
	}

	for key, value := range existingDocuments {
		if value {
			if newVal, ok := updatedDocuments[key]; !ok || !newVal {
				return c.JSON(http.StatusForbidden, fmt.Sprintf("Modification of document '%s' is not allowed", key))
			}
		}
	}

	for key, val := range existingDocuments {
		if val {
			updatedDocuments[key] = val
		}
	}

	updatedGrant.Documents, _ = json.Marshal(updatedDocuments)

	// Prevent changes to specific fields

	updatedGrant.ID = grant.ID
	updatedGrant.DecreeID = grant.DecreeID
	updatedGrant.FileNames = grant.FileNames

	grant = updatedGrant

	if err := db.Save(&grant).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Edit Grant", strconv.Itoa(grant.ID))

	return c.JSON(http.StatusOK, nil)
}

func DeleteGrant(c echo.Context) error {
	db := config.DB()
	minio := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	grantId := c.Request().Header.Get("grantId")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var grant config.Grant
	if err := db.First(&grant, grantId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	// Удаляем файл из MinIO
	bucket, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	prefix := "grant/" + strconv.Itoa(grant.ID) + "/"

	objectCh := minio.ListObjects(c.Request().Context(), bucket, minio2.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for object := range objectCh {
		if object.Err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Error listing objects: %v", object.Err))
		}

		err := minio.RemoveObject(c.Request().Context(), bucket, object.Key, minio2.RemoveObjectOptions{})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, fmt.Sprintf("Failed to delete object %s: %v", object.Key, err))
		}
	}

	// Удаляем запись из базы данных
	if err := db.Delete(&grant).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Delete Grant", strconv.Itoa(grant.ID))

	return c.JSON(http.StatusOK, nil)
}

func CreateSample(c echo.Context) error {
	db := config.DB()

	newData := c.Request().Header.Get("newData")
	accessToken := c.Request().Header.Get("accessToken")

	if newData == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var sample config.Sample

	if err := json.Unmarshal([]byte(newData), &sample); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	if err := db.Create(&sample).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Create Sample", strconv.Itoa(sample.ID))

	return c.JSON(http.StatusOK, nil)
}

func ListSample(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")

	userRole := CheckUserRole(accessToken)
	if userRole != "admin" && userRole != "moderator" {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var samples []config.Sample
	errSamples := db.Where(config.Sample{}).Find(&samples).Error
	if errSamples != nil {
		return c.JSON(http.StatusForbidden, nil)
	}
	return c.JSON(http.StatusOK, samples)
}

func EditSample(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	editedSample := c.Request().Header.Get("editedSample")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var sample config.Sample
	if err := db.First(&sample, sampleId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	var updatedSample config.Sample
	if err := json.Unmarshal([]byte(editedSample), &updatedSample); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	updatedSample.ID = sample.ID
	updatedSample.GrantID = sample.GrantID

	sample = updatedSample

	if err := db.Save(&sample).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Edit Sample", strconv.Itoa(sample.ID))

	return c.JSON(http.StatusOK, nil)
}

func DeleteSample(c echo.Context) error {
	db := config.DB()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")

	user := getUserObject(accessToken)
	if user.Role != "admin" && user.Role != "moderator" || user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var sample config.Sample
	if err := db.First(&sample, sampleId).Error; err != nil {
		return c.JSON(http.StatusNotFound, nil)
	}

	// Удаляем запись из базы данных
	if err := db.Delete(&sample).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, err)
	}

	AddLog(user.ID, "Delete Sample", strconv.Itoa(sample.ID))

	return c.JSON(http.StatusOK, nil)
}
