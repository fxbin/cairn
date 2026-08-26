// Package canonicaljson 提供指纹专用的严格规范化 JSON 序列化：对象键递归按
// UTF-8 字节序排序，数组保持原顺序，拒绝重复键与浮点数，使相同语义的输入
// 恒定映射到相同字节串。它是 EvalSet、凭据策略等冻结指纹的唯一序列化来源；
// 运行观测类的宽松稳定编码仍属于 benchmark 包，两者合同不同不得混用。
//
// Author: fxbin
package canonicaljson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// fingerprintPrefix 分隔域前缀与规范化内容，避免跨域拼接歧义。
const fingerprintPrefix = "\x00"

// fingerprintAlgorithm 是指纹值携带的摘要算法标识。
const fingerprintAlgorithm = "sha256"

// Marshal 把任意可 JSON 序列化的值转换为规范字节串。它先以反射预检拒绝
// Go 层浮点与复数（包括仅值为整数的 float64），再经 token 流重建：逐层对
// 象收集键并检测重复（防御内嵌 JSON 与自定义 MarshalJSON 携带的重复键），
// 数字只接受规范整数字面量，从而满足冻结指纹"整数、布尔、字符串、数组、
// 对象、null"的闭合值域要求。
func Marshal(value any) ([]byte, error) {
	if err := rejectUnsupportedKinds(reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicaljson: encode value: %w", err)
	}
	tree, err := parseCanonical(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	if err := writeValue(&buffer, tree); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Fingerprint 以"版本化域前缀 || NUL || 规范化 JSON"计算 sha256，返回带算法
// 标识的指纹字符串；前缀由调用方携带 schema 世代，格式与仓库既有指纹惯例
// 一致（如 build identity、config fingerprint）。
func Fingerprint(prefix string, value any) (string, error) {
	canonical, err := Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(prefix+fingerprintPrefix), canonical...))
	return fingerprintAlgorithm + ":" + hex.EncodeToString(sum[:]), nil
}

// canonicalValue 是解析后的中间树节点：对象为键到子树的映射并保留逐层去重
// 结论，数组保序，标量为 token 原文或已校验数字文本。
type canonicalValue any

// parseCanonical 从 JSON 流读入单个完整值并构建中间树；流中残留多余内容视
// 为输入合同破坏。重复键、非规范数字与小数/指数字面量全部失败关闭。
func parseCanonical(source io.Reader) (canonicalValue, error) {
	decoder := json.NewDecoder(source)
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonicaljson: trailing data after JSON value")
	}
	return value, nil
}

// decodeValue 递归读取一个完整 JSON 值后委托给按首 token 的解析。
func decodeValue(decoder *json.Decoder) (canonicalValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("canonicaljson: decode token: %w", err)
	}
	return decodeValueAfterToken(decoder, token)
}

// decodeValueAfterToken 从已消费的首 token 开始完成一个值的解析；容器场景
// 中首 token 是开界定符，标量场景中它就是值本身。
func decodeValueAfterToken(decoder *json.Decoder, first json.Token) (canonicalValue, error) {
	switch typed := first.(type) {
	case json.Delim:
		switch typed {
		case '{':
			object := map[string]canonicalValue{}
			for {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("canonicaljson: decode object key: %w", err)
				}
				if delimiter, ok := keyToken.(json.Delim); ok && delimiter == '}' {
					return object, nil
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("canonicaljson: object key is not a string")
				}
				if _, duplicated := object[key]; duplicated {
					return nil, fmt.Errorf("canonicaljson: duplicate object key %q", key)
				}
				item, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = item
			}
		case '[':
			array := []canonicalValue{}
			for {
				next, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("canonicaljson: decode array element: %w", err)
				}
				if delimiter, ok := next.(json.Delim); ok && delimiter == ']' {
					return array, nil
				}
				element, err := decodeValueAfterToken(decoder, next)
				if err != nil {
					return nil, err
				}
				array = append(array, element)
			}
		default:
			return nil, errors.New("canonicaljson: unexpected JSON delimiter")
		}
	case json.Number:
		if err := validateIntegerLiteral(string(typed)); err != nil {
			return nil, err
		}
		return typed, nil
	case string, bool, nil:
		return typed, nil
	default:
		return nil, fmt.Errorf("canonicaljson: unsupported token %T", first)
	}
}

// validateIntegerLiteral 只接受 [0]|[1-9][0-9]* 及其负号形式的规范整数；前导
// 零、小数与指数表示都会让同一数值出现多种字节形态，负零同理。
func validateIntegerLiteral(literal string) error {
	negative := strings.HasPrefix(literal, "-")
	body := strings.TrimPrefix(literal, "-")
	if body == "" {
		return fmt.Errorf("canonicaljson: malformed number literal %q", literal)
	}
	if len(body) > 1 && body[0] == '0' {
		return fmt.Errorf("canonicaljson: non-canonical number literal %q", literal)
	}
	if negative && body == "0" {
		return fmt.Errorf("canonicaljson: non-canonical number literal %q", literal)
	}
	for index := 0; index < len(body); index++ {
		if body[index] < '0' || body[index] > '9' {
			return fmt.Errorf("canonicaljson: non-integer number literal %q", literal)
		}
	}
	return nil
}

// writeValue 紧凑输出中间树：对象键经 sort.Strings 按 UTF-8 字节序排列（Go
// 字符串比较即字节序），数组按原顺序输出，字符串转义固定不做 HTML 转义以
// 保证字节层面的跨平台一致。
func writeValue(buffer *bytes.Buffer, value canonicalValue) error {
	switch typed := value.(type) {
	case map[string]canonicalValue:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeString(buffer, key); err != nil {
				return err
			}
			buffer.WriteByte(':')
			if err := writeValue(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []canonicalValue:
		buffer.WriteByte('[')
		for index, element := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeValue(buffer, element); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case json.Number:
		buffer.WriteString(string(typed))
	case string:
		return writeString(buffer, typed)
	case bool:
		buffer.WriteString(strconv.FormatBool(typed))
	case nil:
		buffer.WriteString("null")
	default:
		return fmt.Errorf("canonicaljson: unsupported canonical value %T", value)
	}
	return nil
}

// writeString 以禁用 HTML 转义的编码器输出 JSON 字符串字面量；尾随换行来自
// Encoder 合同，必须去除以保持紧凑形式。
func writeString(buffer *bytes.Buffer, text string) error {
	var writer strings.Builder
	encoder := json.NewEncoder(&writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(text); err != nil {
		return fmt.Errorf("canonicaljson: encode string: %w", err)
	}
	buffer.WriteString(strings.TrimRight(writer.String(), "\n"))
	return nil
}

// rejectUnsupportedKinds 反射遍历值树，在序列化之前拒绝 float32/float64 与
// 复数类型。仅检查导出字段，与 encoding/json 的可见性视角保持一致；未被
// 反射捕获的自定义序列化输出仍会在解析阶段被数字字面量校验兜底。
func rejectUnsupportedKinds(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		return errors.New("canonicaljson: floating-point values are not allowed in fingerprints; use an integer representation")
	case reflect.Complex64, reflect.Complex128:
		return errors.New("canonicaljson: complex values are not allowed in fingerprints")
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return rejectUnsupportedKinds(value.Elem())
	case reflect.Struct:
		for field := range value.NumField() {
			structField := value.Type().Field(field)
			if structField.PkgPath != "" {
				continue
			}
			if err := rejectUnsupportedKinds(value.Field(field)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			// 字节切片经 encoding/json 成为 base64 字符串，确定性由编码器保证。
			return nil
		}
		for index := range value.Len() {
			if err := rejectUnsupportedKinds(value.Index(index)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Array:
		for index := range value.Len() {
			if err := rejectUnsupportedKinds(value.Index(index)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if err := rejectUnsupportedKinds(iter.Key()); err != nil {
				return err
			}
			if err := rejectUnsupportedKinds(iter.Value()); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}
