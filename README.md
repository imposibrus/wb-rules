# wb-rules

Rule engine for Wiren Board


**Table of Contents**

- [Установка на Wiren Board](#Установка-на-wiren-board)
- [Сборка из исходников](#Сборка-из-исходников)
- [Правила](#Правила)
	- [Пример правил](#Пример-правил)
	- [Определение правил](#Определение-правил)
	- [Определение виртуальных устройств](#Определение-виртуальных-устройств)
	- [Просмотр и выполнение правил](#Просмотр-и-выполнение-правил)
	- [Другие предопределённые функции и переменные](#Другие-предопределённые-функции-и-переменные)
	- [Управление логгированием](#Управление-логгированием)

## Установка на Wiren Board

Пакет wb-rules в репозитории, для установки и обновления надо выполнить
```
apt-get update
apt-get install wb-rules
```

Правила находятся в каталоге ```/etc/wb-rules/```

## Сборка из исходников

Работу с исходными текстами необходимо производить внутри wbdev workspace
(создаётся командой `wbdev update-workspace`).

Сборка исполняемого файла для arm:

```
wbdev hmake clean && wbdev hmake
```

Сборка исполняемого файла для x86_64:

```
wbdev hmake clean && wbdev hmake amd64
```

После выполнения этих команд в папке проекта появляется исполняемый
файл `wb-rules`.

Сборка пакета для Wiren Board:
```
wbdev gdeb
```

## Правила

Правила пишутся на языке ECMAScript 5 (диалектом которого является Javascript) и загружаются из папки `/etc/wb-rules`.

### Пример правил

Пример файла с правилами (`sample1.js`):
```js
// Определяем виртуальное устройство relayClicker
// с параметром enabled типа switch. MQTT-topic параметра -
// /devices/relayClicker/controls/enabled
defineVirtualDevice("relayClicker", {
  title: "Relay Clicker", // Название устройства /devices/relayClicker/meta/name
  cells: {
    // параметры
    enabled: { // /devices/relayClicker/controls/enbabled
      type: "switch",  // тип (.../meta/type)
      value: false     // значение по умолчанию
    }
  }
});

// правило с именем startClicking
defineRule("startClicking", {
  asSoonAs: function () {
    // edge-triggered-правило - выполняется, только когда значение
    // данной функции меняется и при этом становится истинным
    return dev["relayClicker/enabled"] && (dev["uchm121rx/Input 0"] == "0");
  },
  then: function () {
    // выполняется при срабатывании правила
    startTicker("clickTimer", 1000);
  }
});

defineRule("stopClicking", {
  asSoonAs: function () {
    return !dev["relayClicker/enabled"] || dev["uchm121rx/Input 0"] != "0";
  },
  then: function () {
    timers.clickTimer.stop();
  }
});

defineRule("doClick", {
  when: function () {
    // level-triggered правило - срабатывает каждый раз при
    // просмотре данного правила, когда timers.clickTimer.firing
    // истинно. Такое происходит при просмотре правила
    // вследствие срабатывании таймера timers.clickTimer.firing
    return timers.clickTimer.firing;
  },
  then: function () {
    // отправляем значение в /devices/uchm121rx/controls/Relay 0/on
    dev["uchm121rx/Relay 0"] = !dev["uchm121rx/Relay 0"];
  }
});

defineRule("echo", {
  // Срабатывание при изменения значения параметра.
  // Вызывается также при первоначальном просмотре
  // правил, если /devices/wb-w1/controls/00042d40ffff
  // и /devices/wb-w1/controls/00042d40ffff/meta/type
  // были среди retained-значений
  whenChanged: "wb-w1/00042d40ffff",
  then: function (newValue, devName, cellName) {
    // Запуск shell-команды
    runShellCommand("echo " + devName + "/" + cellName + "=" + newValue, {
      captureOutput: true,
      exitCallback: function (exitCode, capturedOutput) {
        log("cmd output: " + capturedOutput);
      }
    });
  }
});

// при необходимости можно определять глобальные функции
function cellSpec(devName, cellName) {
  // используем форматирование строк
  return devName === undefined ? "(no cell)" : "{}/{}".format(devName, cellName);
}

// пример правила, срабатывающего по изменению значений функции
defineRule("funcValueChange2", {
  whenChanged: [
    // Правило срабатывает, когда изменяется значение
    // /devices/somedev/controls/cellforfunc1 или
    // меняется значение выражения dev["somedev/cellforfunc2"] > 3.
    // Также оно срабатывает при первоначальном просмотре
    // правил если хотя бы один из используемых в
    // whenChanged параметров находится среди retained-значений.
    "somedev/cellforfunc1",
    function () {
      return dev["somedev/cellforfunc2"] > 3;
    }
  ],
  then: function (newValue, devName, cellName) {
    // при использовании whenChanged в then-функцию
    // передаётся newValue - значение изменившегося
    // параметра или функции, упомянутой в whenChanged.
    // В случае, когда правило срабатывает
    log("funcValueChange2: {}: {} ({})", cellSpec(devName, cellName),
        newValue, typeof(newValue));
  }
});
```

### Определение правил

Правила определяются при помощи функции `defineRule`.

`defineRule(name, { asSoonAs|when: function() { ... }, then: function () { ... } })` или
`defineRule(name, { whenChanged: ["dev1/name1", "dev2/name2", somefunc, ...], then: function (value, dev, name) { ... })`
задаёт правило. Правила просматриваются при получении значений
параметров по MQTT и срабатывании таймеров (см. `startTimer()`
`startTicker()` ниже). При задании `whenChanged` правило срабатывает
при любых изменениях значений параметров или функций, указанных в списке.
Каждый параметр задаётся в виде "имя устройства/имя параметра". Для краткости
в случае единственного параметра или функции вместо списка можно просто
задать строку "имя устройства/имя параметра".
В функцию, заданную в значении ключа `then`, передаются в качестве
аргументов текущее значение параметра, имя устройства и имя параметра,
изменение которого привело к срабатыванию правила. В случае, если
правило сработало из-за изменения функции, фигурирующей в whenChanged,
в качестве единственного аргумента в then передаётся текущее значение
этой функции. Если срабатывание правила не связано непосредственно
с изменением параметра (например, вызов при инициализации, по таймеру
или через `runRules()`),
`then` вызывается без аргументов, т.е. значением всех
трёх аргументов будет `undefined`.
`whenChanged`-правила вызываются также и при первом
просмотре правил, если фигурирующие непосредственно в списке
или внутри вызываемых функций параметры определены среди retained-значений
(см. подробности ниже в разделе **Просмотр правил**).
`whenChanged` также следует использовать для параметров
типа `pushbutton` - правила, в списке `whenChanged`
которых фигурируют `pushbutton`-параметры, срабатывают
каждый раз при нажатии на кнопку в пользовательском
интерфейсе. При использовании `whenChanged` для кнопок не даётся
никаких гарантий по поводу значения `newValue`, передаваемого в
`then`.

Правила, задаваемые при помощи `asSoonAs`, называются edge-triggered и срабатывают в случае,
когда значение, возвращаемое функцией, заданной в `asSoonAs`, становится истинным при том,
что при предыдущем просмотре данного правила оно было ложным.

Правила, задаваемые при помощи `when`, называются level-triggered,
и срабатывают при каждом просмотре, при котором функция, заданная в `when`, возвращает
истинное значение. При срабатывании правила выполняется функция, заданная
в свойстве `when`.

Отдельный тип правил - cron-правила. Такие правила задаются следующим образом:
```js
defineRule("crontest_hourly", {
  when: cron("@hourly"),
  then: function () {
    log("@hourly rule fired");
  }
});
```
Вместо `@hourly` здесь можно задать любое выражение, допустимое в
стандартном crontab, например, `0 20 * * *` (выполнять правило каждый
день в 20:00). Помимо стандартных выражений допускается использование
ряда расширений,
см. [описание](http://godoc.org/github.com/robfig/cron#hdr-CRON_Expression_Format)
формата выражений используемой cron-библиотеки.

### Объект `dev`

`dev` задаёт доступные параметры и устройства. `dev["abc/def"]` задаёт
параметр `def` устройства `abc`, доступный по MQTT-топику
`/devices/.../controls/...`. Альтернативная нотация -
`dev["abc"]["def"]` (или, что в данном случае то же самое,
dev.abc.def).

Значение параметра зависит от его типа: `switch`, `wo-switch`, `alarm` -
булевский тип, "text" - строковой, остальные известные типы параметров,
кроме уставок диммеров (тип rgb), считаются числовыми, уставки диммеров (тип rgb)
и неизвестные типы параметров - строковыми. Список допустимых типов
параметров см.
[по ссылке](https://github.com/contactless/homeui/blob/contactless/conventions.md).

Не следует использовать объект `dev` вне кода правил. Не следует
присваивать значения параметрам через `dev` вне `then`-функций правил
и функций обработки таймеров (коллбэки `setInterval` /
`setTimeout`). В обоих случаях последствия не определены.

Операция присваивания `dev[...] = ...` в `then`-всегда приводит к
публикации MQTT-сообщения, даже если значение параметра не изменилось.
В случае виртуальных устройств новое значение публикуется в топике
`/devices/.../controls/...`, и соответствующее значение
`dev[...]` изменяется сразу:
```js
defineVirtualDevice("virtdev", {
  // ...
});

defineRule("someRule", {
  when: ...,
  then: function () {
    dev["virtdev/someparam"] = 42; // публикация 42 -> /devices/virtdev/controls/someparam
    log("v={}", dev["virtdev/someparam"]); // всегда выдаёт v=42
  }
});
```

В случае внешних устройств новое значение публикуется в топике
`/devices/.../controls/.../on`, а соответствующее значение
`dev[...]` изменится только после получения ответного значения в
топике `/devices/.../controls/...` от драйвера устройства:
```js
defineRule("anotherRule", {
  when: ...,
  then: function () {
    dev["extdev/someparam"] = 42; // публикация 42 -> /devices/extdev/controls/someparam
    log("v={}", dev["extdev/someparam"]); // выдаёт старое значение
  }
});
```

### Определение виртуальных устройств

`defineVirtualDevice(name, { title: <название>, cells: { описание параметров... } })`
задаёт виртуальное устройство, которое может быть использовано для включения/выключения тех
или иных управляющих алгоритмов и установки их параметров.

Описания параметров - ECMAScript-объект, ключами которого являются имена параметров,
а значениями - описания параметров.
Описание параметра - объект с полями
* `type` - тип, публикуемый в MQTT-топике `/devices/.../controls/.../meta/type` для данного параметра.
* `value` - значение параметра по умолчанию (топик `/devices/.../controls/...`).
* `max` для параметра типа `range` может задавать его максимально допустимое значение.
* `readonly` - когда задано истинное значение, параметр объявляется read-only
  (публикуется `1` в `/devices/.../controls/.../meta/readonly`).

### Просмотр и выполнение правил

В данном разделе подробно рассматривается механизм
просмотра и выполнения правил. Рекомендуется к внимательному
прочтению в том числе в случае возникновения непонятных ситуаций
с несрабатывающими правилами.

Правила просматриваются в следующих случаях:
* при инициализации rule engine после получения всех retained-значений из MQTT;
* при изменении метаданных устройств (добавлении и переименовании устройств);
* при изменении любого параметра, доступного в MQTT (`/devices/+/controls/+`).
  В данном случае в целях оптимизации правила просматриваются избирательно (см. ниже);
* при срабатывании таймера, запущенного при помощи `startTimer()` или
  `startTicker()`.
  В данном случае правила также просматриваются избирательно (см. ниже);
* при явном вызове `runRules()` из обработчика таймера, заданного по
  `setTimeout()` или `setInterval()`.

Для просмотра правил важным является понятие *полного* (complete) параметра.
Параметр считается полным, когда для него по MQTT получены как значение,
так и тип (`.../meta/type`). В отладочном режиме попытки
обращения к неполным параметрам в функциях, фигурирующих
в `when`, `asSoonAs` и `whenChanged` приводят к записи
в лог сообщения *"skipping rule due to incomplete cell"*.

Далее описаны способы просмотра правил различного типа.
Следует обратить внимание на оптимизацию просмотра правил
при получении MQTT-значений и срабатывании таймеров, запущенных
через `startTimer()` или `startTicker()`. Данная оптимизация
может привести к нежелательным результатам, если в условиях
правила фигурируют изменяемые пользовательские глобальные переменные,
т.к. факт доступа к этим переменным не фиксируется
и их изменение может не повлечь за собой просмотр правила
при последующих срабатываниях таймера или получении
MQTT-значений.
В этой связи вместо изменяемых пользовательских глобальных переменных
в условиях правил рекомендуется использовать параметры
виртуальных устройств.

Срабатывание правила означает вызов `then`-функции
данного правила.

Просмотр level-triggered правил (when) осуществляется следующим
образом: вызывается функция, заданная в `when`. Если функция
обращается хотя бы к одному неполному параметру, правило не
выполняется. Если функция не обращалась к неполным параметрам
и вернула истинное значение, правило выполняется. В любом
случае все параметры, доступные через `dev`, доступ к которым
осуществлялся во время выполнения функции, фиксируются, и в
дальнейшем при получении значений параметров из MQTT правило
просматривается только тогда, когда topic полученного сообщения
относится к параметру, хотя бы раз опрашивавшемуся в `when`-функции
данного правила. Аналогичным образом фиксируется доступ
к объекту `timers` - при срабатывании таймеров, запущенных
через `startTimer()` или `startTicker()`, правило просматривается
только в том случае, если его `when`-функция хотя бы раз
обращалась к данному конкретному правилу.

Просмотр edge-triggered правил (asSoonAs) осуществляется следующим
образом: вызывается функция, заданная в `asSoonAs`. Если функция
обращается хотя бы к одному неполному параметру, правило не
выполняется. Если функция не обращалась к неполным параметрам
и вернула истинное значение, и при этом правило просматривается
первый раз, либо при предыдущем просмотре значение функции
было ложным, правило выполняется. В любом случае все параметры,
доступные через `dev`, доступ к которым осуществлялся во время
выполнения функции, фиксируются, и в дальнейшем при получении
значений параметров из MQTT правило просмотривается только тогда,
когда topic полученного сообщения относится к параметру, хотя
бы раз опрашивавшемуся в `asSoonAs`-функции данного правила.
Аналогичным образом фиксируется доступ к объекту `timers` -
при срабатывании таймеров, запущенных через `startTimer()`
или `startTicker()`, правило просматривается
только в том случае, если его `asSoonAs`-функция хотя бы раз
обращалась к данному конкретному правилу.

Просмотр правил, срабатывающих на изменение значения
(`whenChanged`) происходит следующим образом. При просмотре
во время инициализации правило срабатывает, если хотя бы один
из параметров, непосредственно перечисленных в `whenChanged`,
является полным, либо если хотя бы одна из функций, перечисленных
в `whenChanged`, при вызове **не** обращается к неполным параметрам.
При получении MQTT-значений параметров правило срабатывает,
в случае, если выполнено хотя бы одно из следующих условий:
* после прихода сообщения соответствующий параметр является
  полным, изменил своё значение с момента прошлого просмотра
  и непосредственно упомянут в `whenChanged`;
* после прихода сообщения соответствующий параметр является
  полным, имеет тип `pushbutton` и непосредственно упомянут
  в `whenChanged`;
* хотя бы одна из функций, фигурирующих в `whenChanged`,
  не обращается к неполным параметрам и возвращает
  значение, отличное от того, которое она вернула
  при предшествующем просмотре.

Во время работы функций, фигурирующих в `whenChanged`,
доступ к параметрам через `dev` фиксируется и в дальнейшем
при получении значений параметров из MQTT правило просмотривается
только тогда, когда topic полученного сообщения относится
к параметру, хотя бы раз опрашивавшемуся в какой либо
из функций, фигурирующих в `whenChanged` правила, либо
непосредственно упомянутому в `whenChanged`.

При срабатывании таймеров, запущенных через `startTimer()`
или `startTicker()`, `whenChanged`-правила не просматриваются.

Cron-правила обрабатываются отдельно от остальных правил при
наступлении времени, удовлетворяющего заданному в определении правила
cron-выражению.

**Во избежание труднопредсказуемого поведения** в функциях,
фигурирующих в `when`, `asSoonAs` и `whenChanged`
не рекомендуется использовать  side effects, т.е.
менять состояние программы (изменять значение глобальных
переменных, значений параметров, запускать таймеры и т.д.)
Следует особо отметить, что система не даёт никаких
гарантий по тому, сколько раз будут вызываться эти функции
при просмотрах правил.

### Другие предопределённые функции и переменные

`global` - глобальный объект ECMAScript (в браузерном JavaScript
глобальный объект доступен, как window)

`defineAlias(name, "device/param")` задаёт альтернативное имя для параметра.
Например, после выполнения `defineAlias("heaterRelayOn", "Relays/Relay 1");` выражение
`heaterRelayOn = true` означает то же самое, что `dev["Relays/Relay 1"] = true`.

`startTimer(name, milliseconds)`
запускает однократный таймер с указанным именем.

Таймер становится доступным как `timers.<name>`. При срабатывании таймера происходит просмотр правил, при этом `timers.<name>.firing` для этого таймера становится истинным на время этого просмотра.

`startTicker(name, milliseconds)`
запускает периодический таймер с указанным интервалом, который также становится доступным как `timers.<name>`.

Метод `stop()` таймера (обычного или периодического) приводит к его останову.

Объект `timers` устроен таким образом, что `timers.<name>` для любого произвольного
`<name>` всегда возвращает "таймероподобный" объект, т.е. объект с методом
`stop()` и свойством `firing`. Для неактивных таймеров `firing` всегда содержит
`false`, а метод `stop()` ничего не делает.

`"...".format(arg1, arg2, ...)` осуществляет последовательную замену
подстрок `{}` в указанной строке на строковые представления своих
аргументов и возвращает результирующую строку. Например,
`"a={} b={}".format("q", 42)` даёт `"a=q b=42"`. Для включения символа
`{` в строку формата следует использовать `{{`: `"a={} {{}".format("q")`
даёт `"a=q {}"`. Если в списке аргументов `format()` присутствуют лишние
аргументы, они добавляются в конец строки через пробел: `"abc {}:".format(1, 42)`
даёт `"abc 1: 42"`.

`"...".xformat(arg1, arg2, ...)` осуществляет последовательную замену
подстрок `{}` в указанной строке на строковые представления своих
аргументов и возвращает результирующую строку. Например,
`"a={} b={}".xformat("q", 42)` даёт `"a=q b=42"`. Для включения символа
`{` в строку формата следует использовать `\{` (`\\{` внутри
строковой константы ECMAScript): `"a={} \\{}".xformat("q")`
даёт `"a=q {}"` (важно! в `format()`, в отличие от `xformat()`,
для escape используется две фигурные скобки). Кроме того, `xformat()`
позволяет включать в текст результат выполнения произвольных ECMAScript-выражений:
`"Some value: {{dev["abc/def"]}}"`. В этой связи `xformat()`
следует использовать с осторожностью в тех случаях, когда
непривелегированный пользователь может влиять на содержимое
строки формата.

`log.{debug,info,warning,error}(fmt, [arg1 [, ...]])` выводит
сообщение в лог. В зависимости от функции сообщение классифицируется
как отладочное (`debug`), информационное (`info`), предупреждение
(`warning`) или сообщение об ошибке (`error`).  В стандартной
конфигурации, т.е. при использовании syslog, сообщение попадает
`/var/log/syslog`, `/var/log/daemon.log`. Используется форматированный
вывод, как в случае `"...".format(...)`, при этом аргумент `fmt`
выступает в качестве строки формата, т.е. `log.info("a={}", 42)`
выводит в лог строку `a=42`.

Помимо syslog, сообщение дублируется в зависимости от функции в виде
MQTT-сообщения в топике `/wbrules/log/debug`, `/wbrules/log/info`,
`/wbrules/log/warning`, `/wbrules/log/error`. `debug`-сообщения
отправляются в MQTT только в том случае, если включён вывод отладочных сообщений
установкой в 1 параметра `/devices/wbrules/controls/Rule debugging`.
Указанные log-топики используются пользовательским интерфейсом
для консоли сообщений.

`log(fmt, [arg1 [, ...]])` - сокращение для `log.info(...)`

`debug(fmt, [arg1 [, ...]])` - сокращение для `log.debug(...)`

`publish(topic, payload, [QoS [, retain]])`
публикует MQTT-сообщение с указанными topic'ом, содержимым, QoS и значением флага retained.

**Важно:** не следует использовать `publish()` для изменения значения
параметров устройств. Для этого следует использовать объект
`dev` (см. выше).

Пример:
```js
// Публикация non-retained сообщения с содержимым "0" (без кавычек)
// в топике /abc/def/ghi с QoS = 0
publish("/abc/def/ghi", "0");
// То же самое с явным заданием QoS
publish("/abc/def/ghi", "0", 0);
// То же самой с QoS=2
publish("/abc/def/ghi", "0", 2);
// То же самое с retained-флагом
publish("/abc/def/ghi", "0", 2, true);
```

`setTimeout(callback, milliseconds)` запускает однократный таймер,
вызывающий при срабатывании функцию, переданную в качестве аргумента
`callback`. Возвращает положительный целочисленный идентификатор
таймера, который может быть использован в качестве аргумента функции
`clearTimeout()`.

`setInterval(callback, milliseconds)` запускает периодический таймер,
вызывающий при срабатывании функцию, переданную в качестве аргумента
`callback`. Возвращает положительный целочисленный идентификатор
таймера, который может быть использован в качестве аргумента функции
`clearTimeout()`.

`clearTimeout(id)` останавливает таймер с указанным идентификатором.
Функция `clearInterval(id)` является alias'ом `clearTimeout()`.

`runRules()` вызывает обработку правил. Может быть использовано в
обработчиках таймеров.

`spawn(cmd, args, options)` запускает внешний процесс, определяемый
`cmd`.  Необязательный параметр `options` - объект, который может
содержать следующие поля:
* `captureOutput` - если `true`, захватить stdout процесса и передать
  его в виде строки в `exitCallback`
* `captureErrorOutput` - если `true`, захватить stderr процесса и
  передать его в виде строки в `exitCallback`. Если данный параметр не
  задан, то stderr дочернего процесса направляется в stderr процесса
  wb-rules
* `input` - строка, которую следует использовать в качестве
  содержимого stdin процесса
* `exitCallback` - функция, вызываемая при завершении
  процесса. Аргументы функции: `exitCode` - код возврата процесса,
  `capturedOutput` - захваченный stdout процесса в виде строки в
  случае, когда задана опция `captureOutput`, `capturedErrorOutput` -
  захваченный stderr процсса в виде строки в случае, когда задана
  опция `captureErrorOutput`

`runShellCommand(cmd, options)` вызывает `/bin/sh` с указанной
командой следующим образом: `spawn("/bin/sh", ["-c", cmd], options)`.

`readConfig(path)` считывает конфигурационный файл в формате
JSON, находящийся по указанному пути. Генерирует исключение,
если файл не найден, не может быть прочитан или разобран.

### Сервис оповещений

*Важно:* следует учитывать, что в дальнейшем сервис оповещений будет
вынесен в отдельный модуль. Существующий API будет оставлен для
совместимости.

`Notify.sendEmail(to, subject, text)` отправляет почту указанному
адресату (`to`), с указанной темой (`subject`) и содержимым (`text`).

`Notify.sendSMS(to, text)` отправляет SMS на указанный номер (`to`)
с указанным содержимым (`text`).

### Сервис алармов

*Важно:* следует учитывать, что в дальнейшем сервис алармов будет
вынесен в отдельный модуль. Существующий API будет оставлен для
совместимости.

Основная функция:

`Alarms.load(spec)` - загружает блок алармов. `spec` может задавать
либо непосредственно блок алармов в виде JavaScript-объекта, либо
указывать путь к JSON-файлу, содержащему описание алармов.

Каждому блоку алармов соответсвует виртуальное устройство, содержащее
по контролу на каждый аларм, отражающему состояние аларма: 0 = не
активен, 1 = активен.  Также в устройстве присутствует дополнительный
контрол log, используемый для логгирования работы службы алармов.

Загружаемый по умолчанию блок алармов находится в файле
`/etc/wb-rules/alarms.conf`. Этот файл доступен для редактирования
через веб-редактор конфигов.

Пример блока алармов с описанием:

```js
{
  // Название MQTT-устройства блока алармов
  "deviceName": "sampleAlarms",

  // Отображаемое название устройства блока алармов
  "deviceTitle": "Sample Alarms",

  // Описание получателей
  "recipients": [
  {
      // Тип получателя - e-mail
      "type": "email",

      // E-mail адрес получателя
      "to": "someone@example.com",

      // Тема письма (необязательное поле)
      "subject": "alarm!"
    },
    {
      // Ещё один e-mail-получатель
      "type": "email",

      // E-mail адрес получателя
      "to": "anotherone@example.com",

      // Тема письма. {} заменяется на текст сообщения
      "subject": "Alarm: {}"
    },
    {
      // Тип получателя - SMS
      "type": "sms",

      // Номер телефона получателя
      "to": "+78122128506"
    }
  ],

  // Описание алармов
  "alarms": [
    {
      // Название аларма
      "name": "importantDeviceIsOff",

      // Наблюдаемые устройство и контрол
      "cell": "somedev/importantDevicePower",

      // Ожидаемое значение. Аларм срабатывает, если значение контрола становится
      // отличным от expectedValue. Когда значение снова становится равным
      // expectedValue, аларм деактивируется.
      "expectedValue": 1,

      // Сообщение, отправляемое при срабатываении аларма.
      // Если сообщение не указано, оно генерируется автоматически на основе
      // текущего значения контрола.
      "alarmMessage": "Important device is off",

      // Сообщение, отправляемое при деактивации аларма.
      // Если сообщение не указано, оно генерируется автоматически на основе
      // текущего значения контрола.
      "noAlarmMessage": "Important device is back on",
    
      // Интервал отправки сообщений во время активности аларма.
      // Если это поле не указано, то сообщения отправляются только
      // при активации и деактивации аларма.
      "interval": 200
    },
    {
      // Название аларма
      "name": "temperatureOutOfBounds",

      // Наблюдаемые устройство и контрол
      "cell": "somedev/devTemp",

      // Вместо expectedValue можно указать minValue, maxValue либо и minValue, и maxValue.
      // Если значение наблюдаемого контрола становится меньше minValue или больше maxValue,
      // происходит срабатывание аларма. Когда значение возвращается в указанный диапазон,
      // аларм деактивируется.
      "minValue": 10,
      "maxValue": 15,
      
      // Сообщение, отправляемое при срабатываении аларма. {} Заменяется
      // на текущее значение контрола. Возможно использование {{ expr }}
      // для вычисления произвольного JS-выражения (см. "...".xformat(...)).
      "alarmMessage": "Temperature out of bounds, value = {{dev['somedev']['devTemp']}}",

      // Сообщение, отправляемое при деактивации аларма. {} Заменяется
      // на текущее значение контрола. Возможно использование {{ expr }}
      // для вычисления произвольного JS-выражения (см. "...".xformat(...)).
      "noAlarmMessage": "Temperature is within bounds again, value = {}",

      // Интервал отправки сообщений во время активности аларма.
      "interval": 10,

      // Максимальное количество отправляемых сообщений.
      // За каждый период активности аларма отправляется не больше
      // указанного количества сообщений.
      "maxCount": 5
    }
  ]
}
```

### Автоматическая перезагрузка сценариев

При внесении изменений в файлы с правилами происходит автоматическая
перезагрузка изменённых файлов. При перезагрузке глобальное состояние
ECMAScript-движка сохраняется, т.е., например, если глобальная
переменная определена в файле `a.js`, то при изменении файла `b.js` её
значение не изменится. Глобальные переменные и функции, определения
которых удалены из правил, также не удаляются до перезагрузки движка
правил (`service wb-rules restart`). В то же время удаление
определений правил и виртуальных устройств отслеживается и
обрабатываются, т.е. если, например, удалить правило из .js-файла, то
это правило более срабатывать не будет.

### Управление логгированием

Для включения отладочного режима задать порт и опцию `-debug`
в `/etc/default/wb-rules`:
```
WB_RULES_OPTIONS="-debug"
```

Сообщения об ошибках записываются в syslog.
