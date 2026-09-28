package star

import (
	"fmt"
	"strings"

	"github.com/faultbox/Faultbox/internal/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func validateStaticGRPCMock(svc *ServiceDef) error {
	for iface, files := range svc.Mock.Descriptors {
		// Check the fallback against each known method it can actually serve.
		if fallback := svc.Mock.Default[iface]; fallback != nil && !fallback.IsDynamic() {
			resp := fallback.Static()
			if resp.Status == 0 && resp.ContentType != protocol.GRPCRawBodyContentType {
				var validationErr error
				files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
					for i := 0; i < fd.Services().Len(); i++ {
						service := fd.Services().Get(i)
						for j := 0; j < service.Methods().Len(); j++ {
							method := service.Methods().Get(j)
							path := "/" + string(service.FullName()) + "/" + string(method.Name())
							matched := false
							for _, route := range svc.Mock.Routes[iface] {
								if route.Pattern == path || route.Pattern == "/**" || strings.HasSuffix(route.Pattern, "/*") && strings.HasPrefix(path, strings.TrimSuffix(route.Pattern, "*")) {
									matched = true
									break
								}
							}
							if !matched {
								if _, err := protocol.JSONToTypedMessage(files, method.Output(), resp.Body); err != nil {
									validationErr = fmt.Errorf("MOCK_ENCODE_ERROR: mock %s.%s default for %s: %w", svc.Name, iface, path, err)
									return false
								}
							}
						}
					}
					return true
				})
				if validationErr != nil {
					return validationErr
				}
			}
		}
		for _, route := range svc.Mock.Routes[iface] {
			if route.Response.IsDynamic() {
				continue
			}
			resp := route.Response.Static()
			if resp.Status != 0 || resp.ContentType == protocol.GRPCRawBodyContentType {
				continue
			}
			var methods []string
			if !strings.Contains(route.Pattern, "*") {
				methods = append(methods, route.Pattern)
			} else {
				files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
					for i := 0; i < fd.Services().Len(); i++ {
						s := fd.Services().Get(i)
						for j := 0; j < s.Methods().Len(); j++ {
							m := "/" + string(s.FullName()) + "/" + string(s.Methods().Get(j).Name())
							if route.Pattern == "/**" || strings.HasSuffix(route.Pattern, "/*") && strings.HasPrefix(m, strings.TrimSuffix(route.Pattern, "*")) {
								methods = append(methods, m)
							}
						}
					}
					return true
				})
			}
			if len(methods) == 0 {
				return fmt.Errorf("mock %s.%s: no descriptor methods match %s", svc.Name, iface, route.Pattern)
			}
			for _, method := range methods {
				_, out, err := protocol.ResolveMethod(files, method)
				if err == nil {
					_, err = protocol.JSONToTypedMessage(files, out, resp.Body)
				}
				if err != nil {
					return fmt.Errorf("MOCK_ENCODE_ERROR: mock %s.%s %s: %w", svc.Name, iface, method, err)
				}
			}
		}
	}
	return nil
}
