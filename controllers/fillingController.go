package controllers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/labstack/echo/v4"
	minio2 "github.com/minio/minio-go/v7"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gorm.io/gorm"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"park/config"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenthenguyen/docx"
)

func autoFill(userId string, sampleId string) error {
	minioClient := config.MinioClient()

	// Загружаем personalData.json
	personalData, err := getPersonalData(minioClient, userId, sampleId)
	if err != nil {
		return err
	}

	// Получаем список .docx файлов
	docFiles, err := listDocxFiles(minioClient, userId, sampleId)
	if err != nil {
		return err
	}

	// Обрабатываем каждый .docx
	for _, fileName := range docFiles {
		// Загружаем шаблон документа
		docBuffer, err := getDocumentTemplate(minioClient, fileName)
		if err != nil {
			return err
			//println("Ошибка загрузки %s: %v", fileName, err)
			//continue
		}

		// Заполняем документ в памяти
		filledDocBuffer, err := fillTemplate(docBuffer, personalData)
		if err != nil {
			return err
		}


		// Конвертируем заполненный документ в PDF
		pdfBufferStr := LibreSendToQueueSync(func() string {
			buf, err := convertDocxToPDF(filledDocBuffer)
			if err != nil {
				log.Println("LibreOffice convert error:", err)
				return ""
			}
			return base64.StdEncoding.EncodeToString(buf.Bytes())
		})
		if pdfBufferStr == "" {
			return fmt.Errorf("failed to convert PDF via LibreOffice queue")
		}
		decodedPDF, err := base64.StdEncoding.DecodeString(pdfBufferStr)
		if err != nil {
			return fmt.Errorf("base64 decode failed: %v", err)
		}
		pdfBuffer := bytes.NewBuffer(decodedPDF)

		// Сохраняем PDF в MinIO
		newFileName := strings.TrimSuffix(fileName, ".docx") + ".pdf"
		err = saveFileToMinio(minioClient, pdfBuffer, userId, sampleId, path.Base(newFileName))
		if err != nil {
			return err
		}
	}

	return nil
}

func convertDocxToPDF(docxBuffer *bytes.Buffer) (*bytes.Buffer, error) {
	tempDocxFile, err := ioutil.TempFile("", "*.docx")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp .docx file: %v", err)
	}
	defer os.Remove(tempDocxFile.Name())
	defer tempDocxFile.Close()

	_, err = tempDocxFile.Write(docxBuffer.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to write to temp .docx file: %v", err)
	}

	cmd := exec.Command("unoconv", "-f", "pdf", tempDocxFile.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err = cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("unoconv conversion failed: %v\nDetails: %s", err, stderr.String())
	}
	pdfPath := strings.TrimSuffix(tempDocxFile.Name(), filepath.Ext(tempDocxFile.Name())) + ".pdf"

	// BEGIN: async wait for PDF file to appear and be readable (timeout 1 min)
	var fileData []byte
	done := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				data, err := os.ReadFile(pdfPath)
				if err == nil && len(data) > 0 {
					fileData = data
					close(done)
					return
				}
			}
		}
	}()

	select {
	case <-done:
		defer os.Remove(pdfPath)
	case <-time.After(1 * time.Minute):
		close(stop)
		return nil, fmt.Errorf("Ожидание PDF-файла превысило 60 секунд. STDERR: %s", stderr.String())
	}

	return bytes.NewBuffer(fileData), nil
}

func findRequiredFields(accessToken string, sampleId string) error {
	minioClient := config.MinioClient()

	user := getUserObject(accessToken)

	// Загружаем список .docx файлов
	docFiles, err := listDocxFiles(minioClient, strconv.Itoa(user.ID), sampleId)
	if err != nil {
		return err
	}

	// Создайте мапу для хранения найденных полей
	requiredFields := make(map[string]string)

	// Обрабатываем каждый файл, найдя все ключи в формате `{{ключ}}`
	for _, fileName := range docFiles {
		docBuffer, err := getDocumentTemplate(minioClient, fileName)
		if err != nil {
			return err
		}

		doc, err := docx.ReadDocxFromMemory(bytes.NewReader(docBuffer.Bytes()), int64(docBuffer.Len()))
		if err != nil {
			return err
		}
		defer doc.Close()

		content := doc.Editable().GetContent()
		matches := stripAllXMLExceptKeys(content)
		if matches != "" {
			for _, key := range strings.Split(matches, " ") {
				requiredFields[key] = ""
			}
		}

	}

	// Сериализуйте requiredFields как JSON-объект с ключами
	jsonData, err := json.MarshalIndent(requiredFields, "", "  ")
	if err != nil {
		return err
	}

	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	objectName := fmt.Sprintf("requiredFields.json", user.ID, sampleId)
	_, err = minioClient.PutObject(
		context.Background(),
		bucketName,
		objectName,
		bytes.NewReader(jsonData),
		int64(len(jsonData)),
		minio2.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return err
	}

	return nil
}

func saveFilledFields(userId, sampleId string, filledFields map[string]interface{}) error {
	minioClient := config.MinioClient()
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	// Загружаем requiredFields.json
	requiredFieldsPath := fmt.Sprintf("requiredFields.json", userId, sampleId)
	obj, err := minioClient.GetObject(context.Background(), bucketName, requiredFieldsPath, minio2.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("Ошибка загрузки requiredFields.json: %v", err)
	}
	defer obj.Close()

	requiredFieldsData, err := io.ReadAll(obj)
	if err != nil {
		return fmt.Errorf("Ошибка чтения requiredFields.json: %v", err)
	}

	var requiredFields map[string]string
	if err := json.Unmarshal(requiredFieldsData, &requiredFields); err != nil {
		return fmt.Errorf("Ошибка разбора requiredFields.json: %v", err)
	}

	for key := range requiredFields {

		val, exists := filledFields[key]
		if !exists {
			continue
		}

		strVal, ok := val.(string)
		if !ok {
			continue
		}

		if strings.TrimSpace(strVal) == "" {
			continue
		}

		requiredFields[key] = strVal
	}

	// Сохраняем обновленный requiredFields.json
	updatedRequiredFieldsData, err := json.MarshalIndent(requiredFields, "", "  ")
	if err != nil {
		return fmt.Errorf("Ошибка сериализации updatedRequiredFields.json: %v", err)
	}

	_, err = minioClient.PutObject(
		context.Background(),
		bucketName,
		requiredFieldsPath,
		bytes.NewReader(updatedRequiredFieldsData),
		int64(len(updatedRequiredFieldsData)),
		minio2.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return fmt.Errorf("Ошибка сохранения обновленного requiredFields.json: %v", err)
	}

	return nil
}

// ----------MAIN FUNC----------
func FindRequiredFieldsAI(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	user := getUserObject(accessToken)
	if sampleId == "" || user.ID == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Некорректные параметры"})
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)
	res := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if res.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "no sample ID found"})
	} else if userSample.Status == "awaitAI" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "awaiting results"})
	} else if userSample.Status != "startAI" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "incorrect stage"})
	} else {
		userSample.Status = "awaitAI"
		db.Save(&userSample)
	}
	// Run processing in background
	AISendToQueueAsync(func() []byte {
		processFindRequiredFieldsAI(db, accessToken, sampleId, user)
		return nil
	})

	return c.JSON(http.StatusAccepted, map[string]string{"message": "Обработка запущена"})
}

// ProcessFindRequiredFieldsAI performs the full required fields AI processing logic.
func processFindRequiredFieldsAI(db *gorm.DB, accessToken, sampleId string, user config.User) {
	defer func() {
		if r := recover(); r != nil {
			// Try to set status to doneAI if panic occurs
			var userSample config.UserSample
			sampleIdInt, _ := strconv.Atoi(sampleId)
			db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
			userSample.Status = "doneAI"
			db.Save(&userSample)
		}
	}()

	minioClient := config.MinioClient()
	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)
	db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)

	// Step 1: Find required fields
	err := findRequiredFields(accessToken, sampleId)
	if err != nil {
		userSample.Status = "doneAI"
		db.Save(&userSample)
		return
	}

	// Step 2: OCR
	var ocrResults []string
	pdfFiles, err := listPdfFiles(minioClient, strconv.Itoa(user.ID), sampleId)
	if err != nil {
		userSample.Status = "doneAI"
		db.Save(&userSample)
		return
	}
	for _, fileName := range pdfFiles {
		pdfBuffer, err := getPdfFromMinio(minioClient, fileName)
		if err != nil {
			userSample.Status = "doneAI"
			db.Save(&userSample)
			return
		}
		ocrText := OCRSendToQueueSync(func() []byte {
			return scanOcr(pdfBuffer.Bytes()) // игнорируем ошибку
		})
		if ocrText == nil {
			userSample.Status = "doneAI"
			db.Save(&userSample)
		}
		ocrResults = append(ocrResults, string(ocrText))
	}

	// Объединяем весь OCR в один текст
	fullText := strings.Join(ocrResults, " ")

	// Step 3: AI request
	const maxChunkSize = 31000
	var chunks []string

	for start := 0; start < len(fullText); start += maxChunkSize {
		end := start + maxChunkSize
		if end > len(fullText) {
			end = len(fullText)
		}
		chunks = append(chunks, fullText[start:end])
	}

	//var allAIResponses []string

	// Initialize rawFields before use
	rawFields := make(map[string]interface{})

	for _, chunk := range chunks {
		aiRes := GPTSendToQueueSync(func() string {
			return aiRequest([]string{chunk}, user.ID, sampleId)
		})
		var outer struct {
			Result struct {
				Alternatives []struct {
					Message struct {
						Text string `json:"text"`
					} `json:"message"`
				} `json:"alternatives"`
			} `json:"result"`
		}

		if err := json.Unmarshal([]byte(aiRes), &outer); err != nil {
			log.Println("JSON parse error:", err)
			continue
		}

		if len(outer.Result.Alternatives) == 0 {
			continue
		}

		var partialFields map[string]interface{}
		if err := json.Unmarshal([]byte(outer.Result.Alternatives[0].Message.Text), &partialFields); err != nil {
			log.Println("inner JSON parse error:", err)
			continue
		}

		for k, v := range partialFields {
			rawFields[k] = v
		}
	}

	if len(rawFields) == 0 {
		userSample.Status = "doneAI"
		db.Save(&userSample)
		return
	}

	// Step 4: Save  fields
	filledFields := make(map[string]interface{})
	for key, value := range rawFields {
		wrappedKey := "{{" + key + "}}"
		filledFields[wrappedKey] = value
	}

	if err := saveFilledFields(strconv.Itoa(user.ID), sampleId, filledFields); err != nil {
		userSample.Status = "doneAI"
		db.Save(&userSample)
		return
	}

	userSample.Status = "doneAI"
	db.Save(&userSample)
}

// ----------MAIN FUNC----------

// ----------AI FUNCS----------

func aiRequest(docsInfo []string, userId int, sampleId string) string {
	apiURL := "https://llm.api.cloud.yandex.net/foundationModels/v1/completion"
	folderId := os.Getenv("FOLDER_ID_YANDEX")
	token, _ := updateIAMToken()

	requiredFieldsBuffer := getSelectedFile(fmt.Sprintf("requiredFields.json", userId, sampleId))

	// Формируем messages
	var messages []map[string]interface{}
	for i, doc := range docsInfo {
		messages = append(messages, map[string]interface{}{
			"role": "user",
			"text": fmt.Sprintf("OCRDoc %d:\n%s", i+1, doc),
		})
	}
	messages = append(messages, map[string]interface{}{
		"role": "user",
		"text": "JSONRequirements: " + string(requiredFieldsBuffer.Bytes()),
	})
	messages = append(messages, map[string]interface{}{
		"role": "user",
		"text": `Промпт`,
	})

	// Формируем payload
	requestPayload := map[string]interface{}{
		"modelUri": "gpt://" + folderId + "/yandexgpt-32k",
		"completionOptions": map[string]interface{}{
			"stream":      false,
			"temperature": 0.1,
			"maxTokens":   "32000",
			"reasoningOptions": map[string]interface{}{
				"mode": "ENABLED_HIDDEN",
			},
			"reasoningTokens": "123",
		},
		"messages":   messages,
		"jsonObject": true,
	}

	jsonData, err := json.Marshal(requestPayload)
	if err != nil {
		return "Ошибка сериализации JSON"
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "Ошибка создания HTTP-запроса"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "Ошибка отправки запроса"
	}
	defer resp.Body.Close()

	responseData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "Ошибка чтения ответа"
	}

	log.Println(string(responseData))

	return string(responseData)
}

func scanOcr(file []byte) []byte {
	// Разбиваем PDF на страницы

	pages, err := splitPDFInMemory(file)
	if err != nil {
		return nil
	}

	token, err := updateIAMToken()
	if err != nil {
		return nil
	}

	var allResults []string

	// Обрабатываем каждую страницу
	for i, pageBytes := range pages {
		println("processing page", i+1)
		apiResponse, err := sendToYandexOCR(pageBytes, token)
		if err != nil {
			return nil
		}
		// Чистим результат OCR от лишних символов
		cleanApiResponse := strings.ReplaceAll(string(apiResponse), "\\n", " ")
		cleanApiResponse = strings.ReplaceAll(cleanApiResponse, "\\", "")
		cleanApiResponse = strings.ReplaceAll(cleanApiResponse, "\"", "")
		// Добавляем результат в общий список
		allResults = append(allResults, cleanApiResponse)
	}

	// Преобразуем массив строк в JSON
	jsonData, err := json.Marshal(allResults)
	if err != nil {
		return nil
		//panic(err)
	}

	// Создаем файл в памяти (используем bytes.Buffer)
	fileBuffer := bytes.NewBuffer(jsonData)

	return fileBuffer.Bytes()
}

// ----------AI FUNCS----------

// Загружает personalData.json
func getPersonalData(minioClient *minio2.Client, userId string, sampleId string) (map[string]string, error) {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("requiredFields.json", userId, sampleId)

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}

	var personalData map[string]string
	err = json.Unmarshal(data, &personalData)
	if err != nil {
		return nil, err
	}

	return personalData, nil
}

func preFill(message json.RawMessage, companyInfo json.RawMessage) (map[string]interface{}, error) {
	// 1. Разбираем входящий список полей
	var fields map[string]interface{}
	if err := json.Unmarshal(message, &fields); err != nil {
		return nil, fmt.Errorf("ошибка разбора message: %w", err)
	}

	// 2. Разбираем JSON с инфой о компании
	var companyData struct {
		Body struct {
			Docs []map[string]interface{} `json:"docs"`
		} `json:"body"`
	}
	if err := json.Unmarshal(companyInfo, &companyData); err != nil {
		return nil, fmt.Errorf("ошибка разбора companyInfo: %w", err)
	}
	if len(companyData.Body.Docs) == 0 {
		return nil, fmt.Errorf("companyInfo содержит пустой список docs")
	}
	company := companyData.Body.Docs[0] // Берем первый документ

	// 3. Заполняем поля, если они есть в company (рекурсивный поиск)
	for key := range fields {
		cleanKey := strings.TrimSuffix(strings.TrimPrefix(key, "{{"), "}}")
		var search func(string, interface{}) (interface{}, bool)
		search = func(target string, data interface{}) (interface{}, bool) {
			switch val := data.(type) {
			case map[string]interface{}:
				if v, ok := val[target]; ok {
					return v, true
				}
				for _, v := range val {
					if res, found := search(target, v); found {
						return res, true
					}
				}
			case []interface{}:
				for _, item := range val {
					if res, found := search(target, item); found {
						return res, true
					}
				}
			}
			return nil, false
		}

		if val, found := search(cleanKey, company); found {
			if strVal, ok := val.(string); ok {
				strVal = strings.ReplaceAll(strVal, `\"`, `"`)
				fields[key] = strVal
			} else {
				fields[key] = val
			}
		}
	}

	return fields, nil
}

// ----------LIST FILES----------

func listDocxFiles(minioClient *minio2.Client, userId, sampleId string) ([]string, error) {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	prefix := fmt.Sprintf("filling/", userId, sampleId)

	objectCh := minioClient.ListObjects(context.Background(), bucketName, minio2.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	var docFiles []string
	for object := range objectCh {
		if object.Err != nil {
			return nil, object.Err
		}
		if strings.HasSuffix(object.Key, ".docx") {
			docFiles = append(docFiles, object.Key)
		}
	}

	return docFiles, nil
}

func listPdfFiles(minioClient *minio2.Client, userId, sampleId string) ([]string, error) {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	prefix := fmt.Sprintf("manualUploaded/", userId, sampleId)

	objectCh := minioClient.ListObjects(context.Background(), bucketName, minio2.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: false,
	})

	var pdfFiles []string
	for object := range objectCh {
		if object.Err != nil {
			return nil, object.Err
		}
		if strings.HasSuffix(object.Key, ".pdf") {
			pdfFiles = append(pdfFiles, object.Key)
		}
	}

	return pdfFiles, nil
}

// ----------LIST FILES----------

// ----------GET FILES----------

func getDocumentTemplate(minioClient *minio2.Client, fileName string) (*bytes.Buffer, error) {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	obj, err := minioClient.GetObject(context.Background(), bucketName, fileName, minio2.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}

	return bytes.NewBuffer(data), nil
}

func getPdfFromMinio(minioClient *minio2.Client, fileName string) (*bytes.Buffer, error) {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	obj, err := minioClient.GetObject(context.Background(), bucketName, fileName, minio2.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}

	return bytes.NewBuffer(data), nil
}

func getSelectedFile(fileName string) bytes.Buffer {

	minioClient := config.MinioClient()
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	obj, err := minioClient.GetObject(context.Background(), bucketName, fileName, minio2.GetObjectOptions{})
	if err != nil {
		panic(fmt.Sprintf("Ошибка загрузки файла %s: %v", fileName, err))
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		panic(fmt.Sprintf("Ошибка чтения файла %s: %v", fileName, err))
	}

	return *bytes.NewBuffer(data)
}

// ----------GET FILES----------

// Заполняет шаблон в памяти (без временных файлов)
func fillTemplate(docBuffer *bytes.Buffer, personalData map[string]string) (*bytes.Buffer, error) {
	// 1️⃣ Читаем `.docx`
	doc, err := docx.ReadDocxFromMemory(bytes.NewReader(docBuffer.Bytes()), int64(docBuffer.Len()))
	if err != nil {
		return nil, err
	}
	defer doc.Close()

	// 2️⃣ Получаем XML-контент документа
	editable := doc.Editable()
	content := editable.GetContent()

	// 3️⃣ Обрабатываем разорванные плейсхолдеры
	keys := make([]string, 0, len(personalData))
	for key := range personalData {
		cleanKey := strings.TrimSuffix(strings.TrimPrefix(key, "{{"), "}}")
		keys = append(keys, cleanKey)
	}
	content = cleanSplitPlaceholders(content, keys)

	// 4️⃣ Делаем замену `{{ключ}}` на значения
	for key, value := range personalData {
		placeholder := fmt.Sprintf("%s", key)
		if strings.Contains(content, placeholder) {
			content = strings.ReplaceAll(content, placeholder, value)
		}
	}

	// 5️⃣ Записываем обратно в `.docx`
	editable.SetContent(content)
	editable.Replace("<w:body>", "<w:body><w:p><w:r><w:t>", -1)
	editable.Replace("</w:body>", "</w:t></w:r></w:p></w:body>", -1)

	//log.Printf("[DEBUG] Filled DOCX content length: %d bytes", len(content))

	// 6️⃣ Сохраняем результат в память
	outputBuffer := new(bytes.Buffer)
	err = editable.Write(outputBuffer)
	if err != nil {
		return nil, err
	}

	return outputBuffer, nil
}

// Сохраняет PDF в MinIO
func saveFileToMinio(minioClient *minio2.Client, docxBuffer *bytes.Buffer, userId, sampleId, fileName string) error {
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("%s", userId, sampleId, strings.TrimPrefix(fileName, fmt.Sprintf("users/%s/samples/%s/", userId, sampleId)))

	_, err := minioClient.PutObject(
		context.Background(),
		bucketName,
		objectName,
		bytes.NewReader(docxBuffer.Bytes()),
		int64(docxBuffer.Len()),
		minio2.PutObjectOptions{},
	)

	return err
}

// ----------DOCX EDITOR----------

func cleanSplitPlaceholders(content string, keys []string) string {
	// Удаляем XML между { и {, }, и }, а потом весь XML внутри скобок
	var builder strings.Builder
	i := 0
	for i < len(content) {
		if content[i] == '{' {
			// Попробуем найти вторую {
			j := i + 1
			for j < len(content) && content[j] != '{' {
				if content[j] == '<' {
					for j < len(content) && content[j] != '>' {
						j++
					}
				}
				j++
			}
			if j < len(content) && content[j] == '{' {
				// Нашли {{ с XML между
				start := i
				end := j + 1
				keyStart := end

				// Теперь ищем первую }
				k := keyStart
				for k < len(content) && content[k] != '}' {
					k++
				}
				if k < len(content) && content[k] == '}' {
					// Ищем вторую } с учётом XML
					m := k + 1
					for m < len(content) && content[m] != '}' {
						if content[m] == '<' {
							for m < len(content) && content[m] != '>' {
								m++
							}
						}
						m++
					}
					if m < len(content) && content[m] == '}' {
						end = m + 1
						raw := content[start:end]
						// Удаляем ВСЕ XML теги внутри
						cleaned := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(raw, "")
						cleaned = strings.TrimSpace(cleaned)
						builder.WriteString(cleaned)
						i = end
						continue
					}
				}
			}
		}
		builder.WriteByte(content[i])
		i++
	}
	return builder.String()
}

// Удаляет весь XML и оставляет только текст
func stripXML(content string) string {
	// Удаляем декларацию XML, если есть
	content = strings.ReplaceAll(content, "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>", "")

	// Убираем все XML namespace-атрибуты (xmlns:w и подобные)
	reNamespaces := regexp.MustCompile(`\s+xmlns:[a-zA-Z0-9]+="[^"]+"`)
	content = reNamespaces.ReplaceAllString(content, "")

	// Убираем все теги <w:...> (или любые другие с `:` в названии)
	reTags := regexp.MustCompile(`<[^>]+>`)
	content = reTags.ReplaceAllString(content, "")

	// Убираем лишние пробелы и переводы строк
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.ReplaceAll(content, "\r", " ")
	content = strings.Join(strings.Fields(content), " ") // Убираем двойные пробелы

	return content
}

func stripAllXMLExceptKeys(content string) string {
	// Удаляем XML
	cleanedContent := stripXML(content)

	// Ищем все плейсхолдеры {{ключ}}
	reKeys := regexp.MustCompile(`{{(.*?)}}`)
	matches := reKeys.FindAllString(cleanedContent, -1)

	// Если найдено — возвращаем строку из всех найденных ключей
	if len(matches) > 0 {
		return strings.Join(matches, " ")
	}

	return ""
}

// ----------DOCX EDITOR----------

// ----------FRONT VERSION----------

func ManualFileUpload(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()
	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	fileName := c.Request().Header.Get("fileName")

	user := getUserObject(accessToken)
	if accessToken == "" || sampleId == "" {
		return c.JSON(400, map[string]string{"error": "accessToken и sampleId обязательны"})
	}

	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	// Получаем файл из запроса
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(400, map[string]string{"error": "Файл обязателен", "error_description": err.Error()})
	}

	// Проверяем, что файл имеет расширение .pdf
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".pdf") {
		return c.JSON(400, map[string]string{"error": "Допускаются только файлы .pdf"})
	}

	// Открываем файл
	src, err := file.Open()
	if err != nil {
		return c.JSON(500, map[string]string{"error": "Ошибка при открытии файла"})
	}
	defer src.Close()

	// Читаем содержимое файла в память
	fileBuffer := new(bytes.Buffer)
	if _, err := io.Copy(fileBuffer, src); err != nil {
		return c.JSON(500, map[string]string{"error": "Ошибка при чтении файла"})
	}

	// Генерируем путь сохранения
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("%s", user.ID, sampleId, file.Filename)

	// Загружаем файл в MinIO
	_, err = minioClient.PutObject(
		context.Background(),
		bucketName,
		objectName,
		bytes.NewReader(fileBuffer.Bytes()),
		int64(fileBuffer.Len()),
		minio2.PutObjectOptions{ContentType: "application/pdf"},
	)
	if err != nil {
		return c.JSON(500, map[string]string{"error": "Ошибка при загрузке файла в MinIO"})
	}

	sampleIdInt, err := strconv.Atoi(sampleId)

	var userSample config.UserSample
	errUserSample := db.Where(config.UserSample{SampleID: sampleIdInt, UserID: user.ID}).First(&userSample)
	if errUserSample.Error != nil || errUserSample.RowsAffected == 0 {
		return c.JSON(http.StatusFailedDependency, err)
	}

	// Преобразуем ToBeUploaded из JSON в []string
	var toBeUploaded []string
	if len(userSample.ToBeUploaded) > 0 {
		if err := json.Unmarshal(userSample.ToBeUploaded, &toBeUploaded); err != nil {
			return c.JSON(500, map[string]string{"error": "Ошибка чтения ToBeUploaded: " + err.Error()})
		}
	}

	// Удаляем имя загруженного файла
	updatedFiles := make([]string, 0, len(toBeUploaded))
	for _, f := range toBeUploaded {
		if f != fileName {
			updatedFiles = append(updatedFiles, f)
		}
	}
	// Сохраняем обратно в RawMessage
	newRaw, err := json.Marshal(updatedFiles)
	if err != nil {
		return c.JSON(500, map[string]string{"error": "Ошибка сохранения ToBeUploaded: " + err.Error()})
	}
	userSample.ToBeUploaded = newRaw
	if len(userSample.ToBeUploaded) == 2 {
		userSample.Status = "startAI"
	}
	db.Save(&userSample)

	return c.JSON(200, map[string]string{"message": "Файл успешно загружен", "filePath": objectName})
}

func GetFieldsToFill(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")

	user := getUserObject(accessToken)

	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Пользователь не найден или заблокирован"})
	}

	if sampleId == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	userId := strconv.Itoa(user.ID)
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("requiredFields.json", userId, sampleId)

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка получения файла из MinIO", "description": err.Error()})
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		if strings.Contains(err.Error(), "The specified key does not exist.") {
			// Если файл не найден, возвращаем пустой список
			return c.JSON(http.StatusOK, []string{})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка чтения файла", "description": err.Error()})
	}

	var company config.Company
	errCompany := db.Where(config.Company{INN: user.CompanyINN}).First(&company)
	if errCompany.Error != nil || errCompany.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	var filledFields map[string]interface{}
	filledFields, err = preFill(data, company.CardData)

	if _, ok := filledFields["{{ФИО}}"]; ok {
		filledFields["{{ФИО}}"] = user.FullName
	}
	if _, ok := filledFields["{{ОГРН}}"]; ok {
		filledFields["{{ОГРН}}"] = user.CompanyOGRN
	}
	if _, ok := filledFields["{{Должность}}"]; ok {
		filledFields["{{Должность}}"] = "Генеральный Директор"
	}
	if _, ok := filledFields["{{Номер_телефона}}"]; ok {
		filledFields["{{Номер_телефона}}"] = user.PhoneNumber
	}
	if _, ok := filledFields["{{Адрес_электронной_почты}}"]; ok {
		filledFields["{{Адрес_электронной_почты}}"] = user.Email
	}
	if _, ok := filledFields["{{Email}}"]; ok {
		filledFields["{{Email}}"] = user.Email
	}
	if _, ok := filledFields["{{ПолнНаимОПФ}}"]; ok {
		filledFields["{{ПолнНаимОПФ}}"] = slice(company.CardData, "$.body.docs.0.ПолнНаимОПФ")
	}

	if _, ok := filledFields["{{ФО2024}}"]; ok {
		fo := slice(company.CardData, "$.body.docs.0.ФО2024.ВЫРУЧКА")
		if fo != "" && fo != "null" {
			filledFields["{{ФО2024}}"] = fo
		}
	}
	if _, ok := filledFields["{{ФО2023}}"]; ok {
		fo := slice(company.CardData, "$.body.docs.0.ФО2023.ВЫРУЧКА")
		if fo != "" && fo != "null" {
			filledFields["{{ФО2023}}"] = fo
		}
	}
	if _, ok := filledFields["{{ФО2022}}"]; ok {
		fo := slice(company.CardData, "$.body.docs.0.ФО2022.ВЫРУЧКА")
		if fo != "" && fo != "null" {
			filledFields["{{ФО2022}}"] = fo
		}
	}
	if _, ok := filledFields["{{ФО2021}}"]; ok {
		fo := slice(company.CardData, "$.body.docs.0.ФО2021.ВЫРУЧКА")
		if fo != "" && fo != "null" {
			filledFields["{{ФО2021}}"] = fo
		}
	}

	formatter := message.NewPrinter(language.Russian)
	formattedMonth := formatter.Sprintf("%s", time.Now().Format("January"))
	formattedMonth = strings.ToLower(formattedMonth)

	months := map[string]string{
		"january": "января", "february": "февраля", "march": "марта", "april": "апреля",
		"may": "мая", "june": "июня", "july": "июля", "august": "августа",
		"september": "сентября", "october": "октября", "november": "ноября", "december": "декабря",
	}
	if val, ok := months[formattedMonth]; ok {
		formattedMonth = val
	}

	if _, ok := filledFields["{{ТекущЧисло}}"]; ok {
		filledFields["{{ТекущЧисло}}"] = strconv.Itoa(time.Now().Day())
	}
	if _, ok := filledFields["{{ТекущМесяц}}"]; ok {
		filledFields["{{ТекущМесяц}}"] = formattedMonth
	}
	if _, ok := filledFields["{{ТекущДата}}"]; ok {
		filledFields["{{ТекущДата}}"] = time.Now().Format("02.01.2006")
	}

	// удалить дубликаты по ключам
	uniqueFields := make(map[string]interface{})
	for k, v := range filledFields {
		uniqueFields[k] = v
	}
	filledFields = uniqueFields

	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка автозаполнения", "description": err.Error()})
	}

	// Возвращаем результат
	return c.JSON(http.StatusOK, filledFields)
}

func FillRequiredFields(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	var fields map[string]interface{}
	if err := json.NewDecoder(c.Request().Body).Decode(&fields); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error":             "Некорректный JSON в теле запроса",
			"error_description": err.Error(),
		})
	}

	user := getUserObject(accessToken)

	if user.ID == 0 || user.IsSuspended {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Пользователь не найден или заблокирован"})
	}

	if sampleId == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	sampleIdInt, err := strconv.Atoi(sampleId)
	var userSample config.UserSample
	res := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if res.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	} else if userSample.Status != "doneAI" && userSample.Status != "filling" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	userId := strconv.Itoa(user.ID)

	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("requiredFields.json", userId, sampleId)

	// Загружаем существующий файл, если он есть
	existingFields := make(map[string]interface{})
	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err == nil {
		defer obj.Close()
		data, err := io.ReadAll(obj)
		if err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &existingFields) // Пытаемся прочитать, игнорируем ошибку
		}
	}

	// Загружаем requiredFields.json для проверки допустимых ключей
	allowedFields := make(map[string]interface{})
	requiredFieldsObj, err := minioClient.GetObject(context.Background(), bucketName, fmt.Sprintf("users/%s/samples/%s/requiredFields.json", userId, sampleId), minio2.GetObjectOptions{})
	if err == nil {
		defer requiredFieldsObj.Close()
		requiredFieldsData, _ := io.ReadAll(requiredFieldsObj)
		_ = json.Unmarshal(requiredFieldsData, &allowedFields) // Игнорируем ошибку
	}

	// Проверка и валидация JSON из запроса
	newFields := fields

	// Объединяем данные: разрешаем обновлять существующие ключи из requiredFields.json, не добавляем новые
	for key, value := range newFields {
		if _, allowed := allowedFields[key]; allowed {
			existingFields[key] = value
		} else {
			log.Println(map[string]interface{}{
				"key": key, "value": value.(string), "allFields": newFields, "allowedFields": allowedFields,
			})
		}
	}

	// Проверка: все ли ключи заполнены
	for key, value := range existingFields {
		strVal, ok := value.(string)
		if !ok {
			strVal = fmt.Sprintf("%v", value)
		}
		if strings.TrimSpace(strVal) == "" {
			return c.JSON(http.StatusBadRequest, map[string]interface{}{
				"error":          "Не все обязательные поля заполнены",
				"missing_key":    key,
				"fields":         newFields,
				"existingFields": existingFields,
				"value":          value,
			})
		}
	}

	// Сохраняем обратно
	dataToWrite, err := json.MarshalIndent(existingFields, "", "  ")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка сериализации JSON", "description": err.Error()})
	}

	_, err = minioClient.PutObject(
		context.Background(),
		bucketName,
		objectName,
		bytes.NewReader(dataToWrite),
		int64(len(dataToWrite)),
		minio2.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка сохранения JSON в MinIO", "description": err.Error()})
	}

	err = autoFill(userId, sampleId)

	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "error while filling", "description": err.Error()})
	}

	userSample.Status = "filling"
	db.Save(&userSample)

	return c.JSON(http.StatusOK, map[string]string{"message": "Данные успешно обновлены"})
}

func ConfirmFilling(c echo.Context) error {
	db := config.DB()
	user := getUserObject(c.Request().Header.Get("accessToken"))
	sampleId, errConv := strconv.Atoi(c.Request().Header.Get("sampleId"))

	if user.ID == 0 || user.IsSuspended {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	if errConv != nil {
		return c.JSON(http.StatusForbidden, nil)
	}

	var userSample config.UserSample
	err := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleId}).First(&userSample).Error
	if err != nil {
		return c.JSON(http.StatusForbidden, nil)
	}

	if userSample.Status != "filling" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	errFill := fillDocs(user, sampleId)
	if errFill != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "error while filling", "description": err.Error()})
	}

	userSample.Status = "filled"
	db.Save(&userSample)

	return c.JSON(http.StatusOK, nil)
}

func fillDocs(user config.User, sampleId int) error {
	db := config.DB()
	if user.ID == 0 || user.IsSuspended {
		return errors.New("unauth")
	}

	var userSample config.UserSample
	err := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleId}).First(&userSample).Error
	if err != nil {
		return errors.New("err get userSample")
	}

	minioClient := config.MinioClient()
	userIdStr := strconv.Itoa(user.ID)

	// 1. Получение всех .docx из filling/
	docFiles, err := listDocxFiles(minioClient, userIdStr, strconv.Itoa(sampleId))
	if err != nil {
		return errors.New("err get files")
	}

	// 2. Получение requiredFields.json
	requiredFields, err := getPersonalData(minioClient, userIdStr, strconv.Itoa(sampleId))
	if err != nil {
		return errors.New("err get requiredFields")
	}

	// 3. Обработка каждого docx
	for _, fileName := range docFiles {
		docBuffer, err := getDocumentTemplate(minioClient, fileName)
		if err != nil {
			return errors.New("err get document template")
		}

		// Заполнение шаблона
		filledDocBuffer, err := fillTemplate(docBuffer, requiredFields)
		if err != nil {
			return errors.New("unauth")
		}

		// Сохранение обратно в MinIO
		err = saveFileToMinio(minioClient, filledDocBuffer, userIdStr, strconv.Itoa(sampleId), fileName)
		if err != nil {
			return errors.New("err save file")
		}
	}

	return nil
}

func GetZipPDFs(c echo.Context) error {
	db := config.DB()
	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")

	user := getUserObject(accessToken)
	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)
	res := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if res.RowsAffected == 0 {
		return c.JSON(http.StatusForbidden, nil)
	}
	if userSample.Status != "filled" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	minioClient := config.MinioClient()
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")

	userIdStr := strconv.Itoa(user.ID)

	// Собираем файлы из двух папок
	fillingPrefix := fmt.Sprintf("filling/", userIdStr, sampleId)
	manualUploadedPrefix := fmt.Sprintf("manualUploaded/", userIdStr, sampleId)

	var pdfFiles []string

	for _, prefix := range []string{fillingPrefix, manualUploadedPrefix} {
		objectCh := minioClient.ListObjects(context.Background(), bucketName, minio2.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		})
		for object := range objectCh {
			if object.Err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": object.Err.Error()})
			}
			if strings.HasSuffix(strings.ToLower(object.Key), ".pdf") {
				pdfFiles = append(pdfFiles, object.Key)
			}
		}
	}

	if len(pdfFiles) == 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "No PDF files found"})
	}

	// Создаём ZIP архив в памяти
	var zipBuffer bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuffer)

	for _, fileKey := range pdfFiles {
		obj, err := minioClient.GetObject(context.Background(), bucketName, fileKey, minio2.GetObjectOptions{})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error fetching file: " + err.Error()})
		}
		data, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error reading file: " + err.Error()})
		}

		f, err := zipWriter.Create(filepath.Base(fileKey))
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error creating zip entry: " + err.Error()})
		}

		_, err = f.Write(data)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error writing to zip: " + err.Error()})
		}
	}

	if err := zipWriter.Close(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Error closing zip: " + err.Error()})
	}

	return c.Blob(http.StatusOK, "application/zip", zipBuffer.Bytes())
}

func UploadSignedZip(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")

	user := getUserObject(accessToken)
	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)
	errUserSample := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if errUserSample.Error != nil || errUserSample.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}
	if userSample.Status != "filled" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	// Получаем файл из запроса
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Файл обязателен", "description": err.Error()})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при открытии файла", "description": err.Error()})
	}
	defer src.Close()

	fileBuffer := new(bytes.Buffer)
	if _, err := io.Copy(fileBuffer, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении файла", "description": err.Error()})
	}

	userIdStr := strconv.Itoa(user.ID)
	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("signedFiles.zip", userIdStr, sampleId)

	_, err = minioClient.PutObject(
		context.Background(),
		bucketName,
		objectName,
		bytes.NewReader(fileBuffer.Bytes()),
		int64(fileBuffer.Len()),
		minio2.PutObjectOptions{ContentType: "application/zip"},
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при загрузке в MinIO", "description": err.Error()})
	}

	userSample.Status = "signed"
	db.Save(&userSample)

	return c.JSON(http.StatusOK, map[string]string{"message": "Файл успешно загружен"})
}

func GetReadyArchives(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	typeArchive := c.Request().Header.Get("typeArchive")

	if typeArchive != "pdf" && typeArchive != "docx" {
		return c.JSON(http.StatusForbidden, "Invalid type archive")
	}

	user := getUserObject(accessToken)
	if user.ID == 0 || user.IsSuspended {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	if sampleId == "" {
		return c.JSON(http.StatusForbidden, nil)
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)

	errUserSample := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if errUserSample.Error != nil || errUserSample.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	if userSample.Status != "signed" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("signedFiles.zip", user.ID, sampleId)

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении файла из MinIO", "description": err.Error()})
	}
	defer obj.Close()
	fileData, err := io.ReadAll(obj)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении файла из MinIO", "description": err.Error()})
	}

	// Получаем все .docx файлы из filling/
	docxFiles, err := listDocxFiles(minioClient, strconv.Itoa(user.ID), sampleId)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении docx файлов", "description": err.Error()})
	}

	// Создаём ZIP архив из .docx файлов в памяти
	var zipDocxBuffer bytes.Buffer
	zipDocxWriter := zip.NewWriter(&zipDocxBuffer)
	for _, fileKey := range docxFiles {
		obj, err := minioClient.GetObject(context.Background(), bucketName, fileKey, minio2.GetObjectOptions{})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении .docx файла из MinIO", "description": err.Error()})
		}
		data, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении .docx файла из MinIO", "description": err.Error()})
		}
		f, err := zipDocxWriter.CreateHeader(&zip.FileHeader{
			Name:   filepath.Base(fileKey),
			Method: zip.Deflate,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при создании zip-вложения", "description": err.Error()})
		}
		_, err = f.Write(data)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при записи zip-вложения", "description": err.Error()})
		}
	}
	if err := zipDocxWriter.Close(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при закрытии zip-вложения", "description": err.Error()})
	}

	if typeArchive == "pdf" {
		return c.Blob(http.StatusOK, "application/zip", fileData)
	} else {
		return c.Blob(http.StatusOK, "application/zip", zipDocxBuffer.Bytes())
	}

}

func PreviewFill(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()
	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	fileName := c.Request().Header.Get("fileName")

	if fileName == "" || !strings.Contains(fileName, ".pdf") {
		return c.JSON(http.StatusForbidden, "invalid file")
	}

	user := getUserObject(accessToken)
	if user.ID == 0 || user.IsSuspended {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)

	errUserSample := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if errUserSample.Error != nil || errUserSample.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	if userSample.Status != "filling" && userSample.Status != "filled" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("", user.ID, sampleId, fileName)

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении файла из MinIO", "description": err.Error()})
	}
	defer obj.Close()
	fileData, err := io.ReadAll(obj)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении файла из MinIO", "description": err.Error()})
	}

	return c.Blob(http.StatusOK, "application/pdf", fileData)
}

func MailSignedZip(c echo.Context) error {
	db := config.DB()
	minioClient := config.MinioClient()

	accessToken := c.Request().Header.Get("accessToken")
	sampleId := c.Request().Header.Get("sampleId")
	user := getUserObject(accessToken)

	if user.ID == 0 {
		return c.JSON(http.StatusUnauthorized, nil)
	}

	var userSample config.UserSample
	sampleIdInt, _ := strconv.Atoi(sampleId)

	errUserSample := db.Where(config.UserSample{UserID: user.ID, SampleID: sampleIdInt}).First(&userSample)
	if errUserSample.Error != nil || errUserSample.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	if userSample.Status != "signed" {
		return c.JSON(http.StatusForbidden, "invalid step")
	}

	bucketName, _ := os.LookupEnv("MINIO_BUCKET_NAME")
	objectName := fmt.Sprintf("signedFiles.zip", user.ID, sampleId)

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectName, minio2.GetObjectOptions{})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении файла из MinIO", "description": err.Error()})
	}
	defer obj.Close()

	fileData, err := io.ReadAll(obj)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении файла из MinIO", "description": err.Error()})
	}

	// Получаем все .docx файлы из filling/
	docxFiles, err := listDocxFiles(minioClient, strconv.Itoa(user.ID), sampleId)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении docx файлов", "description": err.Error()})
	}

	// Создаём ZIP архив из .docx файлов в памяти
	var zipDocxBuffer bytes.Buffer
	zipDocxWriter := zip.NewWriter(&zipDocxBuffer)
	for _, fileKey := range docxFiles {
		obj, err := minioClient.GetObject(context.Background(), bucketName, fileKey, minio2.GetObjectOptions{})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при получении .docx файла из MinIO", "description": err.Error()})
		}
		data, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при чтении .docx файла из MinIO", "description": err.Error()})
		}
		f, err := zipDocxWriter.CreateHeader(&zip.FileHeader{
			Name:   filepath.Base(fileKey),
			Method: zip.Deflate,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при создании zip-вложения", "description": err.Error()})
		}
		_, err = f.Write(data)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при записи zip-вложения", "description": err.Error()})
		}
	}
	if err := zipDocxWriter.Close(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Ошибка при закрытии zip-вложения", "description": err.Error()})
	}

	var sample config.Sample
	errSample := db.Where(config.Sample{ID: userSample.SampleID}).First(&sample)
	if errSample.Error != nil || errSample.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	var grant config.Grant
	errGrant := db.Where(config.Grant{ID: sample.GrantID}).First(&grant)
	if errGrant.Error != nil || errGrant.RowsAffected == 0 {
		return c.JSON(http.StatusInternalServerError, nil)
	}

	errMail := SendMail(user.Email, "Ваши документы готовы!", grant.Instruction, []struct {
		Name string
		Data []byte
	}{
		{
			Name: "Документы в формате .pdf.zip",
			Data: fileData,
		},
		{
			Name: "Документы в формате .docx.zip",
			Data: zipDocxBuffer.Bytes(),
		},
	})
	if errMail != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	userSample.Status = "sent"
	db.Save(&userSample)

	return c.JSON(http.StatusOK, nil)
}

// ----------FRONT VERSION----------

func updateIAMToken() (string, error) {
	apiURL := "https://iam.api.cloud.yandex.net/iam/v1/tokens"
	oauthToken, _ := os.LookupEnv("YANDEX_TOKEN")

	requestBody, err := json.Marshal(map[string]string{
		"yandexPassportOauthToken": oauthToken,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %v", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	var response struct {
		IAMToken string `json:"iamToken"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %v", err)
	}

	return response.IAMToken, nil
}
