package upyun

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	model "github.com/HFO4/cloudreve/models"
	"github.com/HFO4/cloudreve/pkg/filesystem/fsctx"
	"github.com/HFO4/cloudreve/pkg/filesystem/response"
	"github.com/HFO4/cloudreve/pkg/request"
	"github.com/HFO4/cloudreve/pkg/serializer"
	"github.com/upyun/go-sdk/upyun"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UploadPolicy 又拍云上传策略
type UploadPolicy struct {
	Bucket             string `json:"bucket"`
	SaveKey            string `json:"save-key"`
	Expiration         int64  `json:"expiration"`
	CallbackURL        string `json:"notify-url"`
	ContentLength      uint64 `json:"content-length"`
	ContentLengthRange string `json:"content-length-range,omitempty"`
	AllowFileType      string `json:"allow-file-type,omitempty"`
}

// Driver 又拍云策略适配器
type Driver struct {
	Policy *model.Policy
}

// Get 获取文件
func (handler Driver) Get(ctx context.Context, path string) (response.RSCloser, error) {
	// 给文件名加上随机参数以强制拉取
	path = fmt.Sprintf("%s?v=%d", path, time.Now().UnixNano())

	// 获取文件源地址
	downloadURL, err := handler.Source(
		ctx,
		path,
		url.URL{},
		int64(model.GetIntSetting("preview_timeout", 60)),
		false,
		0,
	)
	if err != nil {
		return nil, err
	}

	// 获取文件数据流
	client := request.HTTPClient{}
	resp, err := client.Request(
		"GET",
		downloadURL,
		nil,
		request.WithContext(ctx),
		request.WithHeader(
			http.Header{"Cache-Control": {"no-cache", "no-store", "must-revalidate"}},
		),
		request.WithTimeout(time.Duration(0)),
	).CheckHTTPResponse(200).GetRSCloser()
	if err != nil {
		return nil, err
	}

	resp.SetFirstFakeChunk()

	// 尝试自主获取文件大小
	if file, ok := ctx.Value(fsctx.FileModelCtx).(model.File); ok {
		resp.SetContentLength(int64(file.Size))
	}

	return resp, nil

}

// Put 将文件流保存到指定目录
func (handler Driver) Put(ctx context.Context, file io.ReadCloser, dst string, size uint64) error {
	defer file.Close()

	up := upyun.NewUpYun(&upyun.UpYunConfig{
		Bucket:   handler.Policy.BucketName,
		Operator: handler.Policy.AccessKey,
		Password: handler.Policy.SecretKey,
	})
	err := up.Put(&upyun.PutObjectConfig{
		Path:   dst,
		Reader: file,
	})

	return err
}

// Delete 删除一个或多个文件，
// 返回未删除的文件，及遇到的最后一个错误
func (handler Driver) Delete(ctx context.Context, files []string) ([]string, error) {
	up := upyun.NewUpYun(&upyun.UpYunConfig{
		Bucket:   handler.Policy.BucketName,
		Operator: handler.Policy.AccessKey,
		Password: handler.Policy.SecretKey,
	})

	var (
		failed       = make([]string, 0, len(files))
		lastErr      error
		currentIndex = 0
		indexLock    sync.Mutex
		failedLock   sync.Mutex
		wg           sync.WaitGroup
		routineNum   = 4
	)
	wg.Add(routineNum)

	// upyun不支持批量操作，这里开四个协程并行操作
	for i := 0; i < routineNum; i++ {
		go func() {
			for {
				// 取得待删除文件
				indexLock.Lock()
				if currentIndex >= len(files) {
					// 所有文件处理完成
					wg.Done()
					indexLock.Unlock()
					return
				}
				path := files[currentIndex]
				currentIndex++
				indexLock.Unlock()

				// 发送异步删除请求
				err := up.Delete(&upyun.DeleteObjectConfig{
					Path:  path,
					Async: true,
				})

				// 处理错误
				if err != nil {
					failedLock.Lock()
					lastErr = err
					failed = append(failed, path)
					failedLock.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	return failed, lastErr
}

// Thumb 获取文件缩略图
func (handler Driver) Thumb(ctx context.Context, path string) (*response.ContentResponse, error) {
	var (
		thumbSize = [2]uint{400, 300}
		ok        = false
	)
	if thumbSize, ok = ctx.Value(fsctx.ThumbSizeCtx).([2]uint); !ok {
		return nil, errors.New("无法获取缩略图尺寸设置")
	}

	thumbParam := fmt.Sprintf("!/fwfh/%dx%d", thumbSize[0], thumbSize[1])
	thumbURL, err := handler.Source(
		ctx,
		path+thumbParam,
		url.URL{},
		int64(model.GetIntSetting("preview_timeout", 60)),
		false,
		0,
	)
	if err != nil {
		return nil, err
	}

	return &response.ContentResponse{
		Redirect: true,
		URL:      thumbURL,
	}, nil
}

// Source 获取外链URL
func (handler Driver) Source(
	ctx context.Context,
	path string,
	baseURL url.URL,
	ttl int64,
	isDownload bool,
	speed int,
) (string, error) {
	// 尝试从上下文获取文件名
	fileName := ""
	if file, ok := ctx.Value(fsctx.FileModelCtx).(model.File); ok {
		fileName = file.Name
	}

	sourceURL, err := url.Parse(handler.Policy.BaseURL)
	if err != nil {
		return "", err
	}

	fileKey, err := url.Parse(path)
	if err != nil {
		return "", err
	}

	sourceURL = sourceURL.ResolveReference(fileKey)

	// 如果是下载文件URL
	if isDownload {
		query := sourceURL.Query()
		query.Add("_upd", fileName)
		sourceURL.RawQuery = query.Encode()
	}

	return handler.signURL(ctx, sourceURL, ttl)
}

func (handler Driver) signURL(ctx context.Context, path *url.URL, TTL int64) (string, error) {
	if !handler.Policy.IsPrivate {
		// 未开启Token防盗链时，直接返回
		return path.String(), nil
	}

	etime := time.Now().Add(time.Duration(TTL) * time.Second).Unix()
	signStr := fmt.Sprintf(
		"%s&%d&%s",
		handler.Policy.OptionsSerialized.Token,
		etime,
		path.Path,
	)
	signMd5 := fmt.Sprintf("%x", md5.Sum([]byte(signStr)))
	finalSign := signMd5[12:20] + strconv.FormatInt(etime, 10)

	// 将签名添加到URL中
	query := path.Query()
	query.Add("_upt", finalSign)
	path.RawQuery = query.Encode()

	return path.String(), nil
}

// Token 获取上传策略和认证Token
func (handler Driver) Token(ctx context.Context, TTL int64, key string) (serializer.UploadCredential, error) {
	// 读取上下文中生成的存储路径和文件大小
	savePath, ok := ctx.Value(fsctx.SavePathCtx).(string)
	if !ok {
		return serializer.UploadCredential{}, errors.New("无法获取存储路径")
	}
	fileSize, ok := ctx.Value(fsctx.FileSizeCtx).(uint64)
	if !ok {
		return serializer.UploadCredential{}, errors.New("无法获取文件大小")
	}

	// 检查文件大小

	// 生成回调地址
	siteURL := model.GetSiteURL()
	apiBaseURI, _ := url.Parse("/api/v3/callback/upyun/" + key)
	apiURL := siteURL.ResolveReference(apiBaseURI)

	// 上传策略
	putPolicy := UploadPolicy{
		Bucket: handler.Policy.BucketName,
		// TODO escape
		SaveKey:            savePath,
		Expiration:         time.Now().Add(time.Duration(TTL) * time.Second).Unix(),
		CallbackURL:        apiURL.String(),
		ContentLength:      fileSize,
		ContentLengthRange: fmt.Sprintf("0,%d", fileSize),
		AllowFileType:      strings.Join(handler.Policy.OptionsSerialized.FileType, ","),
	}

	// 生成上传凭证
	return handler.getUploadCredential(ctx, putPolicy)
}

func (handler Driver) getUploadCredential(ctx context.Context, policy UploadPolicy) (serializer.UploadCredential, error) {
	// 生成上传策略
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return serializer.UploadCredential{}, err
	}
	policyEncoded := base64.StdEncoding.EncodeToString(policyJSON)

	// 生成签名
	elements := []string{"POST", "/" + handler.Policy.BucketName, policyEncoded}
	signStr := handler.Sign(ctx, elements)

	return serializer.UploadCredential{
		Policy: policyEncoded,
		Token:  signStr,
	}, nil
}

// Sign 计算又拍云的签名头
func (handler Driver) Sign(ctx context.Context, elements []string) string {
	password := fmt.Sprintf("%x", md5.Sum([]byte(handler.Policy.SecretKey)))
	mac := hmac.New(sha1.New, []byte(password))
	value := strings.Join(elements, "&")
	mac.Write([]byte(value))
	signStr := base64.StdEncoding.EncodeToString((mac.Sum(nil)))
	return fmt.Sprintf("UPYUN %s:%s", handler.Policy.AccessKey, signStr)
}
