// 本文件定义沙箱套件成功语义的版本门。晋升和执行器共用同一数值，防止
// 旧版仅有完成标记的记录被解释成已经通过可信验证。
//
// Author: fxbin
package sandboxeval

// TrustedResultSchemaVersion 是把 Run Success 绑定完整可信验证证据的首版
// 套件与记录 schema 版本；更早版本不能用于新的晋升或试运行。
const TrustedResultSchemaVersion = 3
