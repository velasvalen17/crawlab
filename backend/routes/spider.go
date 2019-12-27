package routes

import (
	"crawlab/constants"
	"crawlab/database"
	"crawlab/entity"
	"crawlab/model"
	"crawlab/services"
	"crawlab/utils"
	"fmt"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/pkg/errors"
	"github.com/satori/go.uuid"
	"github.com/spf13/viper"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

func GetSpiderList(c *gin.Context) {
	pageNum, _ := c.GetQuery("pageNum")
	pageSize, _ := c.GetQuery("pageSize")
	keyword, _ := c.GetQuery("keyword")
	t, _ := c.GetQuery("type")

	filter := bson.M{
		"name": bson.M{"$regex": bson.RegEx{Pattern: keyword, Options: "im"}},
	}

	if t != "" && t != "all" {
		filter["type"] = t
	}

	page := &entity.Page{}
	page.GetPage(pageNum, pageSize)
	results, count, err := model.GetSpiderList(filter, page.Skip, page.Limit)
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    bson.M{"list": results, "total": count},
	})
}

func GetSpider(c *gin.Context) {
	id := c.Param("id")

	if !bson.IsObjectIdHex(id) {
		HandleErrorF(http.StatusBadRequest, c, "invalid id")
	}

	result, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    result,
	})
}

func PostSpider(c *gin.Context) {
	id := c.Param("id")

	if !bson.IsObjectIdHex(id) {
		HandleErrorF(http.StatusBadRequest, c, "invalid id")
	}

	var item model.Spider
	if err := c.ShouldBindJSON(&item); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}

	if err := model.UpdateSpider(bson.ObjectIdHex(id), item); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func PublishSpider(c *gin.Context) {
	id := c.Param("id")

	if !bson.IsObjectIdHex(id) {
		HandleErrorF(http.StatusBadRequest, c, "invalid id")
	}

	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	services.PublishSpider(spider)

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func PutSpider(c *gin.Context) {
	var spider model.Spider
	if err := c.ShouldBindJSON(&spider); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}

	// 爬虫名称不能为空
	if spider.Name == "" {
		HandleErrorF(http.StatusBadRequest, c, "spider name should not be empty")
		return
	}

	// 判断爬虫是否存在
	if spider := model.GetSpiderByName(spider.Name); spider != nil {
		HandleErrorF(http.StatusBadRequest, c, fmt.Sprintf("spider for '%s' already exists", spider.Name))
		return
	}

	// 设置爬虫类别
	spider.Type = constants.Customized

	// 将FileId置空
	spider.FileId = bson.ObjectIdHex(constants.ObjectIdNull)

	// 创建爬虫目录
	spiderDir := filepath.Join(viper.GetString("spider.path"), spider.Name)
	if utils.Exists(spiderDir) {
		if err := os.RemoveAll(spiderDir); err != nil {
			HandleError(http.StatusInternalServerError, c, err)
			return
		}
	}
	if err := os.MkdirAll(spiderDir, 0777); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}
	spider.Src = spiderDir

	// 添加爬虫到数据库
	if err := spider.Add(); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    spider,
	})
}

func UploadSpider(c *gin.Context) {
	// 从body中获取文件
	uploadFile, err := c.FormFile("file")
	if err != nil {
		debug.PrintStack()
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 如果不为zip文件，返回错误
	if !strings.HasSuffix(uploadFile.Filename, ".zip") {
		debug.PrintStack()
		HandleError(http.StatusBadRequest, c, errors.New("Not a valid zip file"))
		return
	}

	// 以防tmp目录不存在
	tmpPath := viper.GetString("other.tmppath")
	if !utils.Exists(tmpPath) {
		if err := os.MkdirAll(tmpPath, os.ModePerm); err != nil {
			log.Error("mkdir other.tmppath dir error:" + err.Error())
			debug.PrintStack()
			HandleError(http.StatusBadRequest, c, errors.New("Mkdir other.tmppath dir error"))
			return
		}
	}

	// 保存到本地临时文件
	randomId := uuid.NewV4()
	tmpFilePath := filepath.Join(tmpPath, randomId.String()+".zip")
	if err := c.SaveUploadedFile(uploadFile, tmpFilePath); err != nil {
		log.Error("save upload file error: " + err.Error())
		debug.PrintStack()
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 获取 GridFS 实例
	s, gf := database.GetGridFs("files")
	defer s.Close()

	// 判断文件是否已经存在
	var gfFile model.GridFs
	if err := gf.Find(bson.M{"filename": uploadFile.Filename}).One(&gfFile); err == nil {
		// 已经存在文件，则删除
		_ = gf.RemoveId(gfFile.Id)
	}

	// 上传到GridFs
	fid, err := services.UploadToGridFs(uploadFile.Filename, tmpFilePath)
	if err != nil {
		log.Errorf("upload to grid fs error: %s", err.Error())
		debug.PrintStack()
		return
	}

	idx := strings.LastIndex(uploadFile.Filename, "/")
	targetFilename := uploadFile.Filename[idx+1:]

	// 判断爬虫是否存在
	spiderName := strings.Replace(targetFilename, ".zip", "", 1)
	spider := model.GetSpiderByName(spiderName)
	if spider == nil {
		// 保存爬虫信息
		srcPath := viper.GetString("spider.path")
		spider := model.Spider{
			Name:        spiderName,
			DisplayName: spiderName,
			Type:        constants.Customized,
			Src:         filepath.Join(srcPath, spiderName),
			FileId:      fid,
		}
		_ = spider.Add()
	} else {
		// 更新file_id
		spider.FileId = fid
		_ = spider.Save()
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    spider,
	})
}

func UploadSpiderFromId(c *gin.Context) {
	// TODO: 与 UploadSpider 部分逻辑重复，需要优化代码
	// 爬虫ID
	spiderId := c.Param("id")

	// 获取爬虫
	spider, err := model.GetSpider(bson.ObjectIdHex(spiderId))
	if err != nil {
		if err == mgo.ErrNotFound {
			HandleErrorF(http.StatusNotFound, c, "cannot find spider")
		} else {
			HandleError(http.StatusInternalServerError, c, err)
		}
		return
	}

	// 从body中获取文件
	uploadFile, err := c.FormFile("file")
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 如果不为zip文件，返回错误
	if !strings.HasSuffix(uploadFile.Filename, ".zip") {
		debug.PrintStack()
		HandleError(http.StatusBadRequest, c, errors.New("Not a valid zip file"))
		return
	}

	// 以防tmp目录不存在
	tmpPath := viper.GetString("other.tmppath")
	if !utils.Exists(tmpPath) {
		if err := os.MkdirAll(tmpPath, os.ModePerm); err != nil {
			log.Error("mkdir other.tmppath dir error:" + err.Error())
			debug.PrintStack()
			HandleError(http.StatusBadRequest, c, errors.New("Mkdir other.tmppath dir error"))
			return
		}
	}

	// 保存到本地临时文件
	randomId := uuid.NewV4()
	tmpFilePath := filepath.Join(tmpPath, randomId.String()+".zip")
	if err := c.SaveUploadedFile(uploadFile, tmpFilePath); err != nil {
		log.Error("save upload file error: " + err.Error())
		debug.PrintStack()
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 获取 GridFS 实例
	s, gf := database.GetGridFs("files")
	defer s.Close()

	// 判断文件是否已经存在
	var gfFile model.GridFs
	if err := gf.Find(bson.M{"filename": uploadFile.Filename}).One(&gfFile); err == nil {
		// 已经存在文件，则删除
		_ = gf.RemoveId(gfFile.Id)
	}

	// 上传到GridFs
	fid, err := services.UploadToGridFs(uploadFile.Filename, tmpFilePath)
	if err != nil {
		log.Errorf("upload to grid fs error: %s", err.Error())
		debug.PrintStack()
		return
	}

	// 更新file_id
	spider.FileId = fid
	_ = spider.Save()

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func DeleteSpider(c *gin.Context) {
	id := c.Param("id")

	if !bson.IsObjectIdHex(id) {
		HandleErrorF(http.StatusBadRequest, c, "invalid id")
		return
	}

	if err := services.RemoveSpider(id); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func GetSpiderTasks(c *gin.Context) {
	id := c.Param("id")

	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	tasks, err := spider.GetTasks()
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    tasks,
	})
}

func GetSpiderDir(c *gin.Context) {
	// 爬虫ID
	id := c.Param("id")

	// 目录相对路径
	path := c.Query("path")

	// 获取爬虫
	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 获取目录下文件列表
	spiderPath := viper.GetString("spider.path")
	f, err := ioutil.ReadDir(filepath.Join(spiderPath, spider.Name, path))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 遍历文件列表
	var fileList []model.File
	for _, file := range f {
		fileList = append(fileList, model.File{
			Name:  file.Name(),
			IsDir: file.IsDir(),
			Size:  file.Size(),
			Path:  filepath.Join(path, file.Name()),
		})
	}

	// 返回结果
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    fileList,
	})
}

// 爬虫文件管理

type SpiderFileReqBody struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	NewPath string `json:"new_path"`
}

func GetSpiderFile(c *gin.Context) {
	// 爬虫ID
	id := c.Param("id")

	// 文件相对路径
	path := c.Query("path")

	// 获取爬虫
	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 读取文件
	fileBytes, err := ioutil.ReadFile(filepath.Join(spider.Src, path))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
	}

	// 返回结果
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    utils.BytesToString(fileBytes),
	})
}

func PostSpiderFile(c *gin.Context) {
	// 爬虫ID
	id := c.Param("id")

	// 文件相对路径
	var reqBody SpiderFileReqBody
	if err := c.ShouldBindJSON(&reqBody); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}

	// 获取爬虫
	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 写文件
	if err := ioutil.WriteFile(filepath.Join(spider.Src, reqBody.Path), []byte(reqBody.Content), os.ModePerm); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 返回结果
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func PutSpiderFile(c *gin.Context) {
	spiderId := c.Param("id")
	var reqBody SpiderFileReqBody
	if err := c.ShouldBindJSON(&reqBody); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}
	spider, err := model.GetSpider(bson.ObjectIdHex(spiderId))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 文件路径
	filePath := path.Join(spider.Src, reqBody.Path)

	// 如果文件已存在，则报错
	if utils.Exists(filePath) {
		HandleErrorF(http.StatusInternalServerError, c, fmt.Sprintf(`%s already exists`, filePath))
		return
	}

	// 写入文件
	if err := ioutil.WriteFile(filePath, []byte(reqBody.Content), 0777); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func PutSpiderDir(c *gin.Context) {
	spiderId := c.Param("id")
	var reqBody SpiderFileReqBody
	if err := c.ShouldBindJSON(&reqBody); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}
	spider, err := model.GetSpider(bson.ObjectIdHex(spiderId))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 文件路径
	filePath := path.Join(spider.Src, reqBody.Path)

	// 如果文件已存在，则报错
	if utils.Exists(filePath) {
		HandleErrorF(http.StatusInternalServerError, c, fmt.Sprintf(`%s already exists`, filePath))
		return
	}

	// 创建文件夹
	if err := os.MkdirAll(filePath, 0777); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func DeleteSpiderFile(c *gin.Context) {
	spiderId := c.Param("id")
	var reqBody SpiderFileReqBody
	if err := c.ShouldBindJSON(&reqBody); err != nil {
		HandleError(http.StatusBadRequest, c, err)
		return
	}
	spider, err := model.GetSpider(bson.ObjectIdHex(spiderId))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}
	filePath := path.Join(spider.Src, reqBody.Path)
	if err := os.RemoveAll(filePath); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

func RenameSpiderFile(c *gin.Context) {
	spiderId := c.Param("id")
	var reqBody SpiderFileReqBody
	if err := c.ShouldBindJSON(&reqBody); err != nil {
		HandleError(http.StatusBadRequest, c, err)
	}
	spider, err := model.GetSpider(bson.ObjectIdHex(spiderId))
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 原文件路径
	filePath := path.Join(spider.Src, reqBody.Path)
	newFilePath := path.Join(spider.Src, reqBody.NewPath)

	// 如果新文件已存在，则报错
	if utils.Exists(newFilePath) {
		HandleErrorF(http.StatusInternalServerError, c, fmt.Sprintf(`%s already exists`, newFilePath))
		return
	}

	// 重命名
	if err := os.Rename(filePath, newFilePath); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 删除原文件
	if err := os.RemoveAll(filePath); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
	}

	// 同步到GridFS
	if err := services.UploadSpiderToGridFsFromMaster(spider); err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
	})
}

// 爬虫类型
func GetSpiderTypes(c *gin.Context) {
	types, err := model.GetSpiderTypes()
	if err != nil {
		HandleError(http.StatusInternalServerError, c, err)
		return
	}
	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data:    types,
	})
}

func GetSpiderStats(c *gin.Context) {
	type Overview struct {
		TaskCount            int     `json:"task_count" bson:"task_count"`
		ResultCount          int     `json:"result_count" bson:"result_count"`
		SuccessCount         int     `json:"success_count" bson:"success_count"`
		SuccessRate          float64 `json:"success_rate"`
		TotalWaitDuration    float64 `json:"wait_duration" bson:"wait_duration"`
		TotalRuntimeDuration float64 `json:"runtime_duration" bson:"runtime_duration"`
		AvgWaitDuration      float64 `json:"avg_wait_duration"`
		AvgRuntimeDuration   float64 `json:"avg_runtime_duration"`
	}

	type Data struct {
		Overview Overview              `json:"overview"`
		Daily    []model.TaskDailyItem `json:"daily"`
	}

	id := c.Param("id")

	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		log.Errorf(err.Error())
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	s, col := database.GetCol("tasks")
	defer s.Close()

	// 起始日期
	startDate := time.Now().Add(-time.Hour * 24 * 30)
	endDate := time.Now()

	// match
	op1 := bson.M{
		"$match": bson.M{
			"spider_id": spider.Id,
			"create_ts": bson.M{
				"$gte": startDate,
				"$lt":  endDate,
			},
		},
	}

	// project
	op2 := bson.M{
		"$project": bson.M{
			"success_count": bson.M{
				"$cond": []interface{}{
					bson.M{
						"$eq": []string{
							"$status",
							constants.StatusFinished,
						},
					},
					1,
					0,
				},
			},
			"result_count":     "$result_count",
			"wait_duration":    "$wait_duration",
			"runtime_duration": "$runtime_duration",
		},
	}

	// group
	op3 := bson.M{
		"$group": bson.M{
			"_id":              nil,
			"task_count":       bson.M{"$sum": 1},
			"success_count":    bson.M{"$sum": "$success_count"},
			"result_count":     bson.M{"$sum": "$result_count"},
			"wait_duration":    bson.M{"$sum": "$wait_duration"},
			"runtime_duration": bson.M{"$sum": "$runtime_duration"},
		},
	}

	// run aggregation pipeline
	var overview Overview
	if err := col.Pipe([]bson.M{op1, op2, op3}).One(&overview); err != nil {
		if err == mgo.ErrNotFound {
			c.JSON(http.StatusOK, Response{
				Status:  "ok",
				Message: "success",
				Data: Data{
					Overview: overview,
					Daily:    []model.TaskDailyItem{},
				},
			})
			return
		}
		log.Errorf(err.Error())
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	// 后续处理
	successCount, _ := strconv.ParseFloat(strconv.Itoa(overview.SuccessCount), 64)
	taskCount, _ := strconv.ParseFloat(strconv.Itoa(overview.TaskCount), 64)
	overview.SuccessRate = successCount / taskCount
	overview.AvgWaitDuration = overview.TotalWaitDuration / taskCount
	overview.AvgRuntimeDuration = overview.TotalRuntimeDuration / taskCount

	items, err := model.GetDailyTaskStats(bson.M{"spider_id": spider.Id})
	if err != nil {
		log.Errorf(err.Error())
		HandleError(http.StatusInternalServerError, c, err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Status:  "ok",
		Message: "success",
		Data: Data{
			Overview: overview,
			Daily:    items,
		},
	})
}
