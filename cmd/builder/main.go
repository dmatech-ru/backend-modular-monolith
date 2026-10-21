package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Module struct {
	Name     string
	Selected bool
}

const cacheFileName = "dmatech_builder_choice.txt"

func main() {
	// 1. Сканируем папку с модулями
	modulesDir := filepath.Join("internal", "modules")
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		fmt.Printf("❌ Ошибка чтения папки %s: %v\n", modulesDir, err)
		os.Exit(1)
	}

	var modules []Module
	for _, entry := range entries {
		if entry.IsDir() {
			modules = append(modules, Module{Name: entry.Name()})
		}
	}

	if len(modules) == 0 {
		fmt.Println("Не найдено ни одного модуля в internal/modules/")
		os.Exit(0)
	}

	// 2. Восстанавливаем предыдущий выбор из tmp системы
	tmpCachePath := filepath.Join(os.TempDir(), cacheFileName)
	if cacheBytes, err := os.ReadFile(tmpCachePath); err == nil {
		cachedMods := strings.Split(string(cacheBytes), ",")
		cacheMap := make(map[string]bool)
		for _, m := range cachedMods {
			cacheMap[strings.TrimSpace(m)] = true
		}
		for i, mod := range modules {
			if cacheMap[mod.Name] {
				modules[i].Selected = true
			}
		}
	}

	// 3. Переводим терминал в RAW режим
	exec.Command("stty", "-F", "/dev/tty", "cbreak", "min", "1").Run()
	exec.Command("stty", "-F", "/dev/tty", "-echo").Run()

	defer func() {
		exec.Command("stty", "-F", "/dev/tty", "sane").Run()
		exec.Command("stty", "-F", "/dev/tty", "echo").Run()
		fmt.Print("\033[?25h") // Возвращаем курсор обратно
	}()

	currentCursor := 0
	confirmed := false
	reader := bufio.NewReader(os.Stdin)

	clearScreen := func() {
		fmt.Print("\033[H\033[2J") // Полная очистка экрана
		fmt.Println("=============================================")
		fmt.Println(" 🚀 DMATECH - GO BUILDER CONFIG     ")
		fmt.Println("=============================================")
		fmt.Println(" [W/S или Стрелочки] - Навигация")
		fmt.Println(" [Пробел] - Выбрать модуль  |  [Enter] - Запустить")
		fmt.Println(" [Q] - Выход")
		fmt.Println("---------------------------------------------")

		for i, mod := range modules {
			cursor := "  "
			if i == currentCursor {
				cursor = "👉 "
			}

			checkbox := "[ ]"
			if mod.Selected {
				checkbox = "[✅]"
			}
			fmt.Printf("%s%s %s\n", cursor, checkbox, mod.Name)
		}
		fmt.Println("---------------------------------------------")
	}

	fmt.Print("\033[?25l") // Прячем системный курсор

	for {
		clearScreen()

		b, err := reader.ReadByte()
		if err != nil {
			break
		}

		// Выход по кнопке Q
		if b == 'q' || b == 'Q' {
			break
		}

		switch b {
		case 'w', 'W': // Навигация вверх
			if currentCursor > 0 {
				currentCursor--
			}
		case 's', 'S': // Навигация вниз
			if currentCursor < len(modules)-1 {
				currentCursor++
			}
		case ' ': // Пробел — переключает флаг выбора
			modules[currentCursor].Selected = !modules[currentCursor].Selected
		case 10, 13: // Нажатие на ENTER — подтверждаем и запускаем компиляцию
			confirmed = true
		case 27: // Обработка физических стрелочек клавиатуры
			if reader.Buffered() >= 2 {
				b2, _ := reader.ReadByte()
				b3, _ := reader.ReadByte()
				if b2 == 91 {
					switch b3 {
					case 65: // ВВЕРХ
						if currentCursor > 0 {
							currentCursor--
						}
					case 66: // ВНИЗ
						if currentCursor < len(modules)-1 {
							currentCursor++
						}
					}
				}
			}
		}

		if confirmed {
			break
		}
	}

	// Возвращаем терминал в нормальный режим до запуска команд Go
	exec.Command("stty", "-F", "/dev/tty", "sane").Run()
	exec.Command("stty", "-F", "/dev/tty", "echo").Run()
	fmt.Print("\033[?25h")

	if !confirmed {
		fmt.Println("Выход без изменений.")
		return
	}

	// 4. Собираем только реально выбранные модули
	var finalSelected []string
	for _, mod := range modules {
		if mod.Selected {
			finalSelected = append(finalSelected, mod.Name)
		}
	}

	if len(finalSelected) == 0 {
		fmt.Println("Ошибка: Вы не выбрали ни одного модуля для запуска!")
		return
	}

	// Сохраняем кэш в tmp системы
	_ = os.WriteFile(tmpCachePath, []byte(strings.Join(finalSelected, ",")), 0644)

	// 5. Генерируем легальный imports.go в папку cmd/bootstrap
	bootstrapDir := filepath.Join("cmd", "bootstrap")
	importsPath := filepath.Join(bootstrapDir, "imports.go")

	var sb strings.Builder
	sb.WriteString("//go:build dynamic\n\n") // Защитный тег компиляции
	sb.WriteString("package main\n\n")
	for _, name := range finalSelected {
		sb.WriteString(fmt.Sprintf("import _ \"dmatech.ru/internal/modules/%s\"\n", name))
	}

	if err := os.WriteFile(importsPath, []byte(sb.String()), 0644); err != nil {
		fmt.Printf("Не удалось создать файл импортов: %v\n", err)
		return
	}
}
