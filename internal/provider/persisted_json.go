// 本文件实现 Provider 权威记录的严格 JSON 解码：拒绝未知字段、重复键、
// 非 UTF-8、尾随值及非规范编码，避免宽松解码让篡改内容逃出指纹投影。
//
// Author: fxbin
package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// decodeStrictPersistedJSON 严格解码单个持久化 JSON 值；target 必须是目标结构
// 指针。解码结果必须能重新编码为完全相同的单行 JSON，因而任何未知字段、
// 重复键、字段名变体、字段重排或尾随内容都会失败关闭。
func decodeStrictPersistedJSON(encoded []byte, target any) error {
	if !utf8.Valid(encoded) {
		return errors.New("JSON is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("persisted JSON carries a trailing value")
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(encoded, canonical) {
		return errors.New("persisted JSON is not in canonical form")
	}
	return nil
}
