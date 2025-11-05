package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"google.golang.org/protobuf/compiler/protogen"
)

// 插件配置
type PluginConfig struct {
	TemplateFile string // 模板文件路径
	OutputDir    string // 输出目录
	PackageName  string // 生成的包名
}

// 服务信息结构体，用于模板渲染
type ServiceInfo struct {
	PackageName      string // 生成的包名
	ServiceName      string // 服务名称
	ProtoPackageName string // proto包名（用于代码中的类型引用，如 prepare_order.PrepareOrderServiceServer）
	ProtoImportPath  string // proto导入路径（完整路径，用于 import 语句，如 git.dreame.tech/.../gen/proto/pages/prepare_order）
}

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		// 解析插件参数
		var param string
		if gen.Request.Parameter != nil {
			param = *gen.Request.Parameter
		}
		config, err := parsePluginOptions(param)
		if err != nil {
			return fmt.Errorf("解析插件参数失败: %v", err)
		}

		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}

			// 查找服务定义
			for _, service := range f.Services {
				// 生成服务注册文件
				if err := generateServiceRegistry(gen, f, service, config); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// parsePluginOptions 解析插件参数
func parsePluginOptions(param string) (*PluginConfig, error) {
	config := &PluginConfig{
		TemplateFile: "",                     // 必须指定模板文件
		OutputDir:    "local_service_center", // 默认输出目录
		PackageName:  "local_service_center", // 默认包名
	}

	if param == "" {
		return nil, fmt.Errorf("必须指定插件参数，至少需要 template_file")
	}

	// 解析参数，格式: key1=value1,key2=value2
	pairs := strings.Split(param, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		switch key {
		case "template_file":
			config.TemplateFile = value
		case "output_dir":
			config.OutputDir = value
		case "package_name":
			config.PackageName = value
		}
	}

	// 验证必需参数
	if config.TemplateFile == "" {
		return nil, fmt.Errorf("必须指定 template_file 参数")
	}

	return config, nil
}

// loadTemplate 加载模板内容
func loadTemplate(config *PluginConfig) (string, error) {
	// 检查模板文件是否存在
	if _, err := os.Stat(config.TemplateFile); os.IsNotExist(err) {
		return "", fmt.Errorf("模板文件不存在: %s", config.TemplateFile)
	}

	// 读取模板文件
	content, err := os.ReadFile(config.TemplateFile)
	if err != nil {
		return "", fmt.Errorf("读取模板文件失败: %v", err)
	}

	return string(content), nil
}

func generateServiceRegistry(gen *protogen.Plugin, file *protogen.File, service *protogen.Service, config *PluginConfig) error {
	// 获取完整的导入路径（支持嵌套目录）
	protoImportPath := string(file.GoImportPath)

	// 使用 protogen 解析的包名（用于代码中的类型引用）
	protoPackageName := string(file.GoPackageName)

	// 服务名称（去掉 Service 后缀）
	serviceName := strings.TrimSuffix(string(service.Desc.Name()), "Service")

	// 准备模板数据
	data := ServiceInfo{
		PackageName:      config.PackageName,
		ServiceName:      serviceName,
		ProtoPackageName: protoPackageName,
		ProtoImportPath:  protoImportPath,
	}

	// 加载模板
	tmplContent, err := loadTemplate(config)
	if err != nil {
		return fmt.Errorf("加载模板失败: %v", err)
	}

	// 解析模板
	tmpl, err := template.New("service_registry").Parse(tmplContent)
	if err != nil {
		return fmt.Errorf("解析模板失败: %v", err)
	}

	// 生成代码
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("执行模板失败: %v", err)
	}

	// 格式化代码
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("格式化代码失败: %v", err)
	}

	// 生成文件名（转换为小驼峰格式）
	fileName := fmt.Sprintf("%s.go", toCamelCase(serviceName))
	outputPath := filepath.Join(config.OutputDir, fileName)

	// 创建输出文件
	g := gen.NewGeneratedFile(outputPath, "")
	if _, err := g.Write(formatted); err != nil {
		return fmt.Errorf("写入文件失败: %v", err)
	}

	return nil
}

// toCamelCase 将大驼峰转换为小驼峰格式
// 例如: "PrepareOrder" -> "prepareOrder", "Order" -> "order", "User" -> "user"
func toCamelCase(s string) string {
	if len(s) == 0 {
		return s
	}

	// 如果第一个字符是大写字母，将其转为小写
	first := s[0]
	if first >= 'A' && first <= 'Z' {
		// 将第一个字符转为小写，其余保持不变
		return string(first+32) + s[1:]
	}

	// 如果第一个字符已经小写，直接返回
	return s
}
