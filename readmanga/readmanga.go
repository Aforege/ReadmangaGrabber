package readmanga

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/cavaliergopher/grab/v3"
	"github.com/goware/urlx"

	"github.com/lirix360/ReadmangaGrabber/config"
	"github.com/lirix360/ReadmangaGrabber/data"
	"github.com/lirix360/ReadmangaGrabber/history"
	"github.com/lirix360/ReadmangaGrabber/pdf"
	"github.com/lirix360/ReadmangaGrabber/tools"
)

type ServersList []struct {
	Path string `json:"path"`
	Res  bool   `json:"res"`
}

func GetMangaInfo(mangaURL string) (data.MangaInfo, error) {
	var err error
	var mangaInfo data.MangaInfo

	pageBody, err := tools.GetPageCF(mangaURL)
	if err != nil {
		return mangaInfo, err
	}

	chaptersPage, err := goquery.NewDocumentFromReader(pageBody)
	if err != nil {
		return mangaInfo, err
	}

	origTitle := chaptersPage.Find(".original-name").Text()

	if origTitle == "" {
		origTitle = chaptersPage.Find(".eng-name").Text()
	}

	mangaInfo.Title = chaptersPage.Find(".name").Text()
	mangaInfo.OrigTitle = origTitle
	mangaInfo.ImgURL, _ = chaptersPage.Find(".picture-flex-img img").Attr("src")

	chaptersPage.Find(".chapters-link a").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if href != "" {
			chapter := data.ChaptersList{
				Title: strings.TrimSpace(s.Text()),
				URL:   href,
			}
			mangaInfo.Chapters = append(mangaInfo.Chapters, chapter)
		}
	})

	return mangaInfo, nil
}

func DownloadChapter(chapter data.ChaptersList, mangaTitle string) ([]string, error) {
	var imageLinks []string

	locURL, err := urlx.Parse(config.Cfg.Readmanga.MainURL)
	if err != nil {
		return nil, err
	}

	pageBody, err := tools.GetPageCF(locURL.Scheme + "://" + locURL.Host + chapter.URL)
	if err != nil {
		return nil, err
	}

	// Ищем обновленный скрипт инициализации ридера (исправлено под новую верстку)
	r := regexp.MustCompile(`rm_h\.initReader\(\s*\[\[(.+)\]\],\s*(false|true),\s*(\[.+\])`)

	srvList := ServersList{}

	chList := r.FindStringSubmatch(string(pageBody))
	if len(chList) < 4 {
		// Запасной вариант на случай, если данные лежат в window.__MANGABOX_DATA__
		rAlt := regexp.MustCompile(`window\.__MANGABOX_DATA__\s*=\s*\{\s*slides:\s*\[\[(.+)\]\],\s*servers:\s*(\[.+\])`)
		chListAlt := rAlt.FindStringSubmatch(string(pageBody))
		
		if len(chListAlt) < 3 {
			return nil, errors.New("не удалось найти данные глав на странице (структура сайта изменилась)")
		}
		
		json.Unmarshal([]byte(chListAlt[2]), &srvList)
		chList = []string{"", chListAlt[1], "", chListAlt[2]}
	} else {
		json.Unmarshal([]byte(chList[3]), &srvList)
	}

	imageParts := strings.Split(strings.Trim(chList[1], "[]"), "],[")

	for i := 0; i < len(imageParts); i++ {
		tmpParts := strings.Split(imageParts[i], ",")
		if len(tmpParts) < 3 {
			continue
		}
		// Собираем ссылку: базовый путь + имя файла
		imageLinks = append(imageLinks, strings.Trim(tmpParts[0], "\"' ")+strings.Trim(tmpParts[2], "\"' "))
	}

	chapterPath := tools.GetChapterPath(mangaTitle, chapter.Title)

	if _, err := os.Stat(chapterPath); os.IsNotExist(err) {
		err = os.MkdirAll(chapterPath, 0755)
		if err != nil {
			slog.Error("Ошибка при создании папки главы", slog.String("Message", err.Error()))
			return nil, err
		}
	}

	var savedFiles []string

	for _, imgURL := range imageLinks {
		var newImgUrl string

		if strings.HasPrefix(imgURL, "/") {
			refURL, _ := urlx.Parse(config.Cfg.Readmanga.MainURL)
			newImgUrl = refURL.Scheme + "://" + refURL.Host + imgURL
		} else {
			newImgUrl = GetServer(imgURL, srvList)
		}

		filename, err := DlImage(newImgUrl, chapterPath, srvList, 1)
		if err == nil {
			savedFiles = append(savedFiles, filename)
		}
	}

	if config.Cfg.SavePDF {
		pdf.CreatePDF(chapterPath, savedFiles)
	}

	history.SaveHistory(chapter.URL)

	return imageLinks, nil
}

func GetServer(imgURL string, srvList ServersList) string {
	idx := rand.Intn(len(srvList))
	return srvList[idx].Path + imgURL
}

func DlImage(imgURL, chapterPath string, srvList ServersList, retry int) (string, error) {
	maxRetry := 3
	refURL, _ := urlx.Parse(config.Cfg.Readmanga.MainURL)

	client := grab.NewClient()
	req, err := grab.NewRequest(chapterPath, imgURL)
	if err != nil {
		slog.Error(
			"Ошибка при создании запроса для скачивания файла",
			slog.String("Message", err.Error()),
		)
		if retry == maxRetry {
			return "", err
		} else {
			time.Sleep(time.Duration(config.Cfg.Readmanga.TimeoutImage) * time.Millisecond)
			return DlImage(imgURL, chapterPath, srvList, retry+1)
		}
	}

	req.HTTPRequest.Header.Set("Referer", refURL.Scheme+"://"+refURL.Host+"/")

	resp := client.Do(req)
	if resp.Err() != nil {
		if resp.HTTPResponse != nil && resp.HTTPResponse.StatusCode == 404 {
			slog.Error(
				"Ошибка при скачивании страницы",
				slog.String("Message", resp.Err().Error()),
			)
			if retry == maxRetry {
				return "", err
			} else {
				newImgUrl := GetServer(imgURL, srvList)
				time.Sleep(time.Duration(config.Cfg.Readmanga.TimeoutImage) * time.Millisecond)
				return DlImage(newImgUrl, chapterPath, srvList, retry+1)
			}
		} else {
			slog.Error(
				"Ошибка при скачивании страницы",
				slog.String("Message", resp.Err().Error()),
			)
			if retry == maxRetry {
				return "", err
			} else {
				time.Sleep(time.Duration(config.Cfg.Readmanga.TimeoutImage) * time.Millisecond)
				return DlImage(imgURL, chapterPath, srvList, retry+1)
			}
		}
	}

	return resp.Filename, nil
}
