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

// registryOpts 由 --service-registry_opt 解析；输出根目录由 protoc 的 --service-registry_out 决定。
type registryOpts struct {
	TemplatePath string // 必填：Go 模板文件路径
	PackageName  string // 生成文件 package 子句
	Subdir       string // 相对 service-registry_out 的子目录（如 local_service_center）
}

type serviceInfo struct {
	PackageName      string
	ServiceName      string
	ProtoPackageName string
	ProtoImportPath  string
}

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		opt, err := parseRegistryOpts(pluginParameter(gen))
		if err != nil {
			return err
		}

		tmpl, err := loadRegistryTemplate(opt.TemplatePath)
		if err != nil {
			return err
		}

		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			for _, svc := range f.Services {
				if err := generateServiceRegistry(gen, f, svc, tmpl, opt); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func pluginParameter(gen *protogen.Plugin) string {
	if gen.Request.Parameter != nil {
		return *gen.Request.Parameter
	}
	return ""
}

// parseRegistryOpts: comma-separated key=value. Keys: template (required), package, subdir.
func parseRegistryOpts(param string) (*registryOpts, error) {
	o := &registryOpts{
		PackageName: "local_service_center",
		Subdir:      "local_service_center",
	}
	if strings.TrimSpace(param) == "" {
		return nil, fmt.Errorf("service-registry: need options, at least template=<path>")
	}

	for _, pair := range strings.Split(param, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("service-registry: invalid option %q (want key=value)", pair)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || v == "" {
			return nil, fmt.Errorf("service-registry: empty key or value in %q", pair)
		}
		switch k {
		case "template":
			o.TemplatePath = v
		case "package":
			o.PackageName = v
		case "subdir":
			o.Subdir = v
		default:
			return nil, fmt.Errorf("service-registry: unknown option %q", k)
		}
	}

	if o.TemplatePath == "" {
		return nil, fmt.Errorf("service-registry: template=<path> is required")
	}
	return o, nil
}

func loadRegistryTemplate(path string) (*template.Template, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("service-registry: template file not found: %s", path)
		}
		return nil, fmt.Errorf("service-registry: read template: %w", err)
	}
	tmpl, err := template.New("service_registry").Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("service-registry: parse template: %w", err)
	}
	return tmpl, nil
}

func generateServiceRegistry(gen *protogen.Plugin, file *protogen.File, service *protogen.Service, tmpl *template.Template, opt *registryOpts) error {
	protoImportPath := string(file.GoImportPath)
	protoPackageName := string(file.GoPackageName)
	serviceName := strings.TrimSuffix(string(service.Desc.Name()), "Service")

	data := serviceInfo{
		PackageName:      opt.PackageName,
		ServiceName:      serviceName,
		ProtoPackageName: protoPackageName,
		ProtoImportPath:  protoImportPath,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("service-registry: execute template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("service-registry: format: %w", err)
	}

	rel := filepath.Join(opt.Subdir, fmt.Sprintf("%s.go", toCamelCase(serviceName)))
	g := gen.NewGeneratedFile(rel, "")
	if _, err := g.Write(formatted); err != nil {
		return fmt.Errorf("service-registry: write %s: %w", rel, err)
	}
	return nil
}

func toCamelCase(s string) string {
	if len(s) == 0 {
		return s
	}
	if c := s[0]; c >= 'A' && c <= 'Z' {
		return string(c+32) + s[1:]
	}
	return s
}
