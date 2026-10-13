# Примечания и источники

К [сравнению подходов](COMPARISON.md). Срез нашего кода: `a1eed878d698b7721c63dd221d97a7951465659f`, 13.10.2026.

## Что проверить в нашем коде

Это результаты чтения кода в данном обзоре, не новые интеграционные тесты. В предыдущей Pi-сессии также сообщалось о воспроизведённых ошибках кэша, коллизий и аудита; здесь их повторно не запускали.

| Проблема | Где смотреть | Проверка |
|---|---|---|
| Сессия не привязана к владельцу | `litellm/pii_bridge.py:131–159,204–216,428–440` | Разные пользователи с одинаковым session ID не должны получать общий словарь |
| Потерянная или повреждённая таблица считается новой | `vendor/palena/internal/mapping/redis.go:172–205,254–265` | Удаление таблицы между ходами должно остановить продолжение, а не переиспользовать прежние номера |
| Разные значения могут одновременно получить одинаковый номер | `vendor/palena/internal/text/handler.go:185–203`, `strategy.go:139–155`; `internal/mapping/redis.go:55–60` | Отправить одновременно разные новые имена. Тест одного повторяющегося имени этот случай не покрывает |
| Кэш не учитывает изменение таблицы | `litellm/pii_bridge.py:246–276,316–327,428–457` | После удаления таблицы или обнаружения нового имени старый результат не должен молча использоваться. Отдельно проверить отказ Redis/Palena при заполненном кэше |
| Не все поля и маршруты обрабатываются | `litellm/pii_walk.py:100–114,224–253`, ограничения в README | Проверить авторизованные запросы, вложения, JSON-ключи и числа. Неподдержанное нужно блокировать |
| ПД могут попасть в поля журнала | `litellm/pii_leak.py:84–92`, `pii_bridge.py:358–364` | Синтетический email в JSON-ключе или session ID не должен попасть в журнал |
| Удаление и целостность журнала описаны слишком сильно | `vendor/palena/internal/mapping/redis.go:240–251`, `litellm/pii_audit.py:97–108` | Redis DEL не удаляет все резервные копии. Цепочка хешей без внешней контрольной записи не обнаруживает удалённый хвост или полное переписывание |

Ещё два расхождения в документации: диаграмма DESIGN запрещает compact, хотя ниже он описан как поддерживаемый; `config.real.yaml` оставляет `turn_off_message_logging: false`, тогда как OBSERVABILITY описывает production с отключённым журналированием сообщений. Код и эти документы в рамках подготовки сравнения не менялись.

## Дополнительные аналоги

| Решение | Почему не вошло в основной список кандидатов |
|---|---|
| **Scrimward** | Есть агентные API и потоковое восстановление. Но новый vault создаётся на запрос; ключ режима AES-SIV не сохраняется. Обычный файловый словарь возможен, но не даёт готового разделения пользователей и конкурентного доступа. Секреты тоже восстанавливаются |
| **Klovys99** | Словарь в SQLite переживает restart, но общий для сессий. Часть связей ответов хранится в памяти. Anthropic HTTP работает без streaming; в прочитанном коде восстановленные ответы печатаются в stdout |
| **og-local** | Обозначения постоянны в пределах процесса, обратная таблица — на запрос. После restart меняется случайная основа обозначений. Остальные API-пути проходят без обработки. BSL-лицензия ограничивает предоставление hosted multi-tenant сервиса третьим лицам |
| **Proxymus Privé** | Есть локальные детекторы, Messages/Chat и потоки; сессионная таблица только в памяти, native Responses нет. System по умолчанию не маскируется |
| **LLM Guard** | Библиотека, а не готовый агентный шлюз. Vault — список пар в памяти; долговременное хранение и протоколы нужно добавить. Репозиторий на дату обзора архивирован |

## Что проверено, а что нет

- Прочитаны первичные документы и выбранные исходники. Общего теста всех решений на одинаковых задачах не было.
- В Scrimward отдельно проверено восстановление шифротокена без таблицы при сохранённом ключе и невозможность такого восстановления новым объектом с новым ключом.
- В LiteLLM отдельно исполнены методы numbered-token replacement/unmask на синтетических данных: два текста получили одинаковый `PERSON_1`, а обратное значение перезаписалось. Это проверка функции, не полный запрос через LiteLLM и Presidio. Вывод нельзя переносить на все режимы маскирования.
- Авторские тесты и показатели моделей не считаются нашими результатами. Отсутствие подтверждения функции не означает, что её точно нет.

Рабочие отчёты и снимки находятся в `/tmp/pii-comparison-review/`; временный каталог может быть удалён системой. Ниже сохранены ссылки на исследованные версии.

## Что взято из предыдущего исследования

Pi-сессия `01a131af-2566-71cc-9b98-2eeb66ae6975` и её файл `PII-development-reading-list.md`:

- **MemPrivacy / Whistledown:** постоянные соответствия и связность разговора — отдельная задача от поиска ПД. Статьи не доказывают поддержку наших протоколов, русского языка и параллельных запросов.
- **Русская PII-модель и benchmark redmadrobot:** предметный кандидат для проверки русского; требуется обработка длинного текста и объединение фрагментов сущностей.
- **Privacy Filter / GLiNER2-PII:** кандидаты для сравнения, но не подтверждённые русскоязычные замены. Не путать GLiNER вообще с конкретным GLiNER2-PII и его списком языков обучения.
- **PrivacyLens / AgentDojo:** проверять получателя и назначение восстановленных данных, включая вредоносные инструкции в результатах инструментов.
- **TAB:** полезен для проверки разных упоминаний одного человека, но английские юридические тексты не заменяют наши русские и английские файлы.
- **LOPSIDED / исследования полезности подмен:** сравнивать обозначения с вымышленными именами на реальных задачах. Генеративное восстановление не подходит вместо точной подстановки в исполняемые команды.

Ссылки: [MemPrivacy](https://arxiv.org/abs/2605.09530), [Whistledown](https://arxiv.org/abs/2511.13319), [русская модель](https://huggingface.co/redmadrobot-rnd/rubert-base-pii-ner), [русский benchmark](https://huggingface.co/datasets/redmadrobot-rnd/pii_benchmark), [GLiNER2-PII](https://huggingface.co/fastino/gliner2-privacy-filter-PII-multi), [PrivacyLens](https://github.com/SALT-NLP/PrivacyLens), [AgentDojo](https://github.com/ethz-spylab/agentdojo), [TAB](https://github.com/NorskRegnesentral/text-anonymization-benchmark), [LOPSIDED](https://arxiv.org/abs/2510.27016).

Эти исследовательские выводы переиспользованы из указанной сессии; нового запуска моделей или перепроверки статей в этой редакции не было.

## Исследованные версии

### Шлюзы

- **[S1] SecurityMasker:** [`4ceba47`](https://github.com/ura14h/SecurityMasker/tree/4ceba47cf2f1846c3f7a74f267a5aeeb235d41b3). [Совместимость и ограничения](https://github.com/ura14h/SecurityMasker/blob/4ceba47cf2f1846c3f7a74f267a5aeeb235d41b3/docs/reference/compatibility.md), [session identity](https://github.com/ura14h/SecurityMasker/blob/4ceba47cf2f1846c3f7a74f267a5aeeb235d41b3/src/securitymasker/gateway/session.py), [store и TTL](https://github.com/ura14h/SecurityMasker/blob/4ceba47cf2f1846c3f7a74f267a5aeeb235d41b3/src/securitymasker/sessions/store.py). Авторский restart-тест: `tests/unit/test_sqlite_store.py`.
- **[S2] PrivAiTe:** [`40ee53f`](https://github.com/crp4222/PrivAiTe/tree/40ee53f559de2877faca7a0bf6649ca08162dfe6). [Gateway docs](https://github.com/crp4222/PrivAiTe/blob/40ee53f559de2877faca7a0bf6649ca08162dfe6/docs/gateway.md), [scrub](https://github.com/crp4222/PrivAiTe/blob/40ee53f559de2877faca7a0bf6649ca08162dfe6/privaite/gateway/scrub.py), [routes](https://github.com/crp4222/PrivAiTe/blob/40ee53f559de2877faca7a0bf6649ca08162dfe6/privaite/gateway/routes.py).
- **[S3] Scrimward:** [`eae1f87`](https://github.com/aezizhu/scrimward/tree/eae1f87346cc68eef30ad5d5f3ac811aee9e19b5). [Vault и AES-SIV](https://github.com/aezizhu/scrimward/blob/eae1f87346cc68eef30ad5d5f3ac811aee9e19b5/scrimward/vault.py), [создание vault на запрос](https://github.com/aezizhu/scrimward/blob/eae1f87346cc68eef30ad5d5f3ac811aee9e19b5/scrimward/proxy.py#L302-L317).
- **[S4] Klovys99:** [`c4e2d94`](https://github.com/Korbicorp/klovys99/tree/c4e2d94e7cd10d6dec6b274e9cedc2869e05b271). [SQLite](https://github.com/Korbicorp/klovys99/blob/c4e2d94e7cd10d6dec6b274e9cedc2869e05b271/internal/anonymizer/sqlite_store.go), [restoration и debug output](https://github.com/Korbicorp/klovys99/blob/c4e2d94e7cd10d6dec6b274e9cedc2869e05b271/internal/proxy/providers/response_restore.go), `internal/proxy/providers/{anthropic,openai}.go` того же commit.
- **[S5] og-local:** [`64abc19`](https://github.com/outgate-ai/og-local/tree/64abc197bbd0dd5f3ae1b9a5ba7e622929147e30). [Генерация токенов](https://github.com/outgate-ai/og-local/blob/64abc197bbd0dd5f3ae1b9a5ba7e622929147e30/internal/pii/redactor.go), [время жизни mapping](https://github.com/outgate-ai/og-local/blob/64abc197bbd0dd5f3ae1b9a5ba7e622929147e30/internal/redact/redact.go), README и LICENSE того же среза. Проверены эти функции и документация, не все адаптеры.
- **[S6] Skyflow + Kong:** [`3c1a4f4`](https://github.com/SkyflowFoundry/skyflow-gateway-kong/tree/3c1a4f4af23056aa0fd5e1918115622172d4daa9), handler 0.7.0. [Реализация selectors, restoration и SSE](https://github.com/SkyflowFoundry/skyflow-gateway-kong/blob/3c1a4f4af23056aa0fd5e1918115622172d4daa9/plugin/kong/plugins/skyflow-ai-data-control/handler.lua), [Kong reference](https://developer.konghq.com/plugins/skyflow-ai-data-control/reference/). Конкретную частную поставку/цену vault не проверяли.
- **[S7] LangSmith:** [canonical Data policy](https://docs.langchain.com/langsmith/llm-gateway-data-policy); [зафиксированный MDX `b592e8f`](https://github.com/langchain-ai/docs/blob/b592e8f91eb8401271dfd7f013228d002f925ce3/src/langsmith/llm-gateway-data-policy.mdx). Это версия документации, не серверного кода; в том же срезе `llm-gateway-api-formats.mdx` и `llm-gateway-how-it-works.mdx`.
- **[S8] Proxymus Privé:** [`c762841`](https://github.com/keefar/proxymus-prive/tree/c762841c08e31c8316ddf55870b02e2e7fb39fd2): `apf/proxy.py`, `vault.py`, `unmasker.py`, `secrets.py` и README.

### Компоненты

- **[C1] LiteLLM:** [`6b7d192`](https://github.com/BerriAI/litellm/tree/6b7d1928b84a5ff618b958b199b52836ebe45499), [Presidio hook, особенно numbered tokens](https://github.com/BerriAI/litellm/blob/6b7d1928b84a5ff618b958b199b52836ebe45499/litellm/proxy/guardrails/guardrail_hooks/presidio.py#L797-L845); [generic guardrail API contract](https://github.com/BerriAI/litellm/blob/6b7d1928b84a5ff618b958b199b52836ebe45499/litellm/types/proxy/guardrails/guardrail_hooks/generic_guardrail_api.py). Этот upstream-срез не тождественен версии из нашего lockfile.
- **[C2] Palena:** [`e2d669b`](https://github.com/PalenaAI/palena-litellm-pseudonymizer/tree/e2d669bf0f95b69b42b8453e2b6e2acd3ad437ee), `internal/session/derive.go`, `internal/mapping/redis.go`, `internal/httpapi/types.go`, `deploy/helm/palena-litellm-pseudonymizer/values.yaml`; [failure modes](https://github.com/PalenaAI/palena-litellm-pseudonymizer/blob/e2d669bf0f95b69b42b8453e2b6e2acd3ad437ee/context/FAILURE_MODES.md). Design-файлы местами описывают намерения; для выбора session ID использована реализация `derive.go`.
- **[C3] Presidio / Invisio:** [`2523c7b`](https://github.com/data-privacy-stack/presidio/tree/2523c7b74a469270c5c78bb253f140eafca21e31), [sample](https://github.com/data-privacy-stack/presidio/tree/2523c7b74a469270c5c78bb253f140eafca21e31/docs/samples/deployments/openai-anonymaztion-and-deanonymaztion-best-practices). На дату чтения прежний Microsoft repo перенаправляет в Data Privacy Stack. Для spaCy/GLiNER — реализации `SpacyNlpEngine` и `GLiNERRecognizer` этого среза.
- **[C4] PII Shield:** [`9927ed5`](https://github.com/vikasgautam18/pii-shield/tree/9927ed500a71edff19b969ab2ce96a776da78856), [README: middleware и stores](https://github.com/vikasgautam18/pii-shield/blob/9927ed500a71edff19b969ab2ce96a776da78856/README.md), `pii_shield/session_store.py`. Redis для app registration нельзя автоматически считать реализацией общего middleware SessionStore.
- **[C5] LLM Guard:** [`168c103`](https://github.com/protectai/llm-guard/tree/168c1034ffdb33837e7ae6fd6a16b80567c1be03), [Vault](https://github.com/protectai/llm-guard/blob/168c1034ffdb33837e7ae6fd6a16b80567c1be03/llm_guard/vault.py). Статус archived проверен по GitHub API на дату обзора.

### Протоколы и детекция

- **[P1] OpenAI:** [conversation state](https://developers.openai.com/api/docs/guides/conversation-state), [reasoning](https://developers.openai.com/api/docs/guides/reasoning), [compaction](https://developers.openai.com/api/docs/guides/compaction). Ручная передача истории, server-side chaining и standalone compact — разные режимы.
- **[P2] Anthropic:** [thinking](https://platform.claude.com/docs/en/build-with-claude/thinking), [preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking), [compaction thinking blocks](https://platform.claude.com/docs/en/build-with-claude/compaction-thinking-blocks). Нельзя обобщать ограничения одной модели/режима на все исторические версии.
- **[D1] Детекторы:** [Privacy Filter model card](https://huggingface.co/openai/privacy-filter), [Gitleaks](https://github.com/gitleaks/gitleaks), Presidio [C3]. Их опубликованные показатели не выдаются за наш benchmark.
- **[N] Наш код:** HEAD `a1eed878d698b7721c63dd221d97a7951465659f`, [исходники](https://github.com/zotho/pii-gateway/tree/a1eed878d698b7721c63dd221d97a7951465659f), локальные `README.md`, `docs/DESIGN.md`, `docs/OBSERVABILITY.md`, `litellm/*`, `vendor/palena/*`, `tests/*`. Lockfile содержит LiteLLM 1.105.0, Presidio Analyzer 2.2.364, spaCy 3.8.16. Это версии зафиксированных зависимостей, не проверка уже запущенного окружения.
