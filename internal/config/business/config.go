// Package business 读取并校验静态 Agent 业务配置。
package business

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

var (
	defaultTemperature = contracts.NewCanonicalDecimalV1(2, 1)
	defaultTopP        = contracts.NewCanonicalDecimalV1(1, 0)
)

// Agent 是一个已经完成默认值、校验和规范化的静态 Agent 配置。
type Agent struct {
	Name            string
	Description     string
	CatalogID       string
	TaskTimeout     time.Duration
	ExecutionConfig contracts.ExecutionConfigV1
}

// Config 保存启动时冻结的 Agent 配置集合。
type Config struct {
	agents map[contracts.AgentID]Agent
}

type document struct {
	Agents []agentDocument `json:"agents"`
}

type agentDocument struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	CatalogID       string          `json:"catalog_id"`
	TaskTimeout     string          `json:"task_timeout"`
	ExecutionConfig json.RawMessage `json:"execution_config"`
}

// Load 从单个 JSON 文件读取业务配置。
func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, errors.New("load business config: path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open business config: %w", err)
	}
	defer file.Close()

	config, err := Parse(file)
	if err != nil {
		return Config{}, fmt.Errorf("load business config: %w", err)
	}
	return config, nil
}

// Parse 严格解析、校验并冻结一份 Agent 业务配置。
func Parse(reader io.Reader) (Config, error) {
	if reader == nil {
		return Config{}, errors.New("decode business config: reader is required")
	}
	var raw document
	if err := decodeStrict(reader, &raw); err != nil {
		return Config{}, fmt.Errorf("decode business config: %w", err)
	}
	if raw.Agents == nil || len(raw.Agents) == 0 {
		return Config{}, errors.New("validate business config: agents must contain at least one agent")
	}

	config := Config{agents: make(map[contracts.AgentID]Agent, len(raw.Agents))}
	for index, source := range raw.Agents {
		agent, err := parseAgent(source)
		if err != nil {
			return Config{}, fmt.Errorf("validate business config: agents[%d]: %w", index, err)
		}
		agentID := agent.ExecutionConfig.Agent.AgentID
		if _, exists := config.agents[agentID]; exists {
			return Config{}, fmt.Errorf("validate business config: duplicate agent_id %q", agentID)
		}
		config.agents[agentID] = agent
	}
	return config, nil
}

// Lookup 返回指定 Agent 的冻结配置。
func (c Config) Lookup(agentID contracts.AgentID) (Agent, bool) {
	agent, ok := c.agents[agentID]
	if !ok {
		return Agent{}, false
	}
	copyConfig, err := taskruntime.NormalizeExecutionConfigV1(agent.ExecutionConfig)
	if err != nil {
		return Agent{}, false
	}
	agent.ExecutionConfig = copyConfig
	return agent, true
}

// Agents 返回按 agent_id 排序的配置副本。
func (c Config) Agents() []Agent {
	agents := make([]Agent, 0, len(c.agents))
	for agentID := range c.agents {
		agent, _ := c.Lookup(agentID)
		agents = append(agents, agent)
	}
	slicesSortAgents(agents)
	return agents
}

func parseAgent(source agentDocument) (Agent, error) {
	if source.Name == "" || source.Description == "" || source.CatalogID == "" {
		return Agent{}, errors.New("name, description, and catalog_id are required")
	}
	if source.TaskTimeout == "" {
		return Agent{}, errors.New("task_timeout is required")
	}
	taskTimeout, err := time.ParseDuration(source.TaskTimeout)
	if err != nil || taskTimeout <= 0 {
		return Agent{}, errors.New("task_timeout must be a positive Go duration")
	}
	if len(source.ExecutionConfig) == 0 || bytes.Equal(bytes.TrimSpace(source.ExecutionConfig), []byte("null")) {
		return Agent{}, errors.New("execution_config is required")
	}
	if err := requireExecutionConfigFields(source.ExecutionConfig); err != nil {
		return Agent{}, err
	}

	var executionConfig contracts.ExecutionConfigV1
	if err := decodeStrict(bytes.NewReader(source.ExecutionConfig), &executionConfig); err != nil {
		return Agent{}, fmt.Errorf("decode execution_config: %w", err)
	}
	if err := applyGenerationDefaults(source.ExecutionConfig, &executionConfig); err != nil {
		return Agent{}, err
	}
	executionConfig, err = taskruntime.NormalizeExecutionConfigV1(executionConfig)
	if err != nil {
		return Agent{}, err
	}
	return Agent{
		Name:            source.Name,
		Description:     source.Description,
		CatalogID:       source.CatalogID,
		TaskTimeout:     taskTimeout,
		ExecutionConfig: executionConfig,
	}, nil
}

func decodeStrict(reader io.Reader, target any) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read JSON document: %w", err)
	}
	if !utf8.Valid(data) {
		return errors.New("JSON document must contain valid UTF-8")
	}
	if err := validateJSONDocument(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("document is required")
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func validateJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, "$", true); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("document is required")
		}
		return err
	}
	if _, err := decoder.Token(); err == nil {
		return errors.New("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string, root bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("%s must not be null", path)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", path)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("%s contains duplicate member %q", path, key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, path+"."+key, false); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		index := 0
		for decoder.More() {
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), false); err != nil {
				return err
			}
			index++
		}
		_, err := decoder.Token()
		return err
	default:
		if root {
			return errors.New("document must be a JSON object")
		}
		return fmt.Errorf("%s has an unexpected JSON delimiter", path)
	}
}

func applyGenerationDefaults(raw json.RawMessage, config *contracts.ExecutionConfigV1) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("inspect execution_config defaults: %w", err)
	}
	model, err := objectField(value, "model")
	if err != nil {
		return err
	}
	params, err := objectField(model, "generation_params")
	if err != nil {
		return err
	}
	if _, exists := params["temperature"]; !exists {
		config.Model.GenerationParams.Temperature = defaultTemperature
	}
	if _, exists := params["top_p"]; !exists {
		config.Model.GenerationParams.TopP = defaultTopP
	}
	if _, exists := params["max_output_tokens"]; !exists {
		config.Model.GenerationParams.MaxOutputTokens = 4096
	}
	return nil
}

func objectField(parent map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	raw, exists := parent[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("execution_config.%s is required", name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("execution_config.%s must be an object", name)
	}
	return object, nil
}

func requireExecutionConfigFields(raw json.RawMessage) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return errors.New("execution_config must be an object")
	}
	return requireStructFields(reflect.TypeFor[contracts.ExecutionConfigV1](), value, "execution_config")
}

func requireStructFields(structType reflect.Type, value map[string]json.RawMessage, path string) error {
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		raw, exists := value[jsonName]
		currentPath := path + "." + jsonName
		if !exists {
			if generationDefaultField(currentPath) {
				continue
			}
			return fmt.Errorf("%s is required", currentPath)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must not be null", currentPath)
		}
		if field.Type == reflect.TypeFor[contracts.CanonicalJSONSchema]() ||
			field.Type == reflect.TypeFor[contracts.CanonicalDecimalV1]() {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Struct:
			var child map[string]json.RawMessage
			if err := json.Unmarshal(raw, &child); err != nil || child == nil {
				return fmt.Errorf("%s must be an object", currentPath)
			}
			if err := requireStructFields(field.Type, child, currentPath); err != nil {
				return err
			}
		case reflect.Slice:
			if field.Type.Elem().Kind() != reflect.Struct || field.Type.Elem() == reflect.TypeFor[contracts.CanonicalJSONSchema]() {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil || items == nil {
				return fmt.Errorf("%s must be an array", currentPath)
			}
			for itemIndex, item := range items {
				var child map[string]json.RawMessage
				if err := json.Unmarshal(item, &child); err != nil || child == nil {
					return fmt.Errorf("%s[%d] must be an object", currentPath, itemIndex)
				}
				if err := requireStructFields(field.Type.Elem(), child, fmt.Sprintf("%s[%d]", currentPath, itemIndex)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func generationDefaultField(path string) bool {
	switch path {
	case "execution_config.model.generation_params.temperature",
		"execution_config.model.generation_params.top_p",
		"execution_config.model.generation_params.max_output_tokens":
		return true
	default:
		return false
	}
}

func slicesSortAgents(agents []Agent) {
	for index := 1; index < len(agents); index++ {
		for cursor := index; cursor > 0 && agents[cursor].ExecutionConfig.Agent.AgentID < agents[cursor-1].ExecutionConfig.Agent.AgentID; cursor-- {
			agents[cursor], agents[cursor-1] = agents[cursor-1], agents[cursor]
		}
	}
}
