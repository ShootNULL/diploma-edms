package controllers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/bhmj/jsonslice"
	"github.com/labstack/echo/v4"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
)


// splitPDFInMemory разбивает PDF на страницы и возвращает массив []byte, где каждый элемент — это страница
func splitPDFInMemory(fileBytes []byte) ([][]byte, error) {
	// Создаем временный файл для pdfcpu
	tmpFile, err := ioutil.TempFile("", "*.pdf")
	if err != nil {
		return nil, err
		//return nil, fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Записываем PDF во временный файл
	if _, err := tmpFile.Write(fileBytes); err != nil {
		return nil, err
		//return nil, fmt.Errorf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Разбиваем PDF на страницы
	outDir, err := ioutil.TempDir("", "pdfpages")
	if err != nil {
		return nil, err
		//return nil, fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(outDir)

	// Используем api.Split для разбиения PDF
	if err := api.SplitFile(tmpFile.Name(), outDir, 1, nil); err != nil {
		return nil, err
		//return nil, fmt.Errorf("failed to split PDF: %v", err)
	}

	// Читаем каждую страницу
	files, err := ioutil.ReadDir(outDir)
	if err != nil {
		return nil, err
		//return nil, fmt.Errorf("failed to read output dir: %v", err)
	}

	var pages [][]byte
	for _, file := range files {
		pageBytes, err := ioutil.ReadFile(filepath.Join(outDir, file.Name()))
		if err != nil {
			return nil, err
			//return nil, fmt.Errorf("failed to read page file: %v", err)
		}
		pages = append(pages, pageBytes)
	}

	return pages, nil
}

func sendToYandexOCR(fileBytes []byte, token string) ([]byte, error) {
	apiURL := "https://ocr.api.cloud.yandex.net/ocr/v1/recognizeText"
	folderID, _ := os.LookupEnv("FOLDER_ID_YANDEX")

	// Кодируем файл в Base64
	encodedFile := base64.StdEncoding.EncodeToString(fileBytes)

	// Формируем JSON-запрос
	requestBody, err := json.Marshal(map[string]interface{}{
		"content":       encodedFile,
		"mimeType":      "application/pdf",
		"languageCodes": []string{"ru"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %v", err)
	}

	// Создаём запрос
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-folder-id", folderID)
	req.Header.Set("x-data-logging-enabled", "true")

	// Отправляем запрос
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Читаем ответ
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	response := []byte(slice(body, "$.result.textAnnotation.fullText"))
	//println(string(body))
	// Возвращаем ответ как есть
	return response, nil
}

func slice(json []byte, path string) string {
	res, _ := jsonslice.Get(json, path)

	if len(res) < 3 {
		return string(res)
	}
	return string(res)[1 : len(res)-1]
}
