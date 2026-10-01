# Decisões registradas

Estas decisões são propostas de implementação para revisão; `SPEC.md` prevalece em caso de divergência. Detalhes definitivos, limitações e trade-offs entram em `ARCHITECTURE.md` durante a execução.

| ID | Decisão | Motivo e verificação |
| --- | --- | --- |
| D01 | `Money` usa `int64` em centavos, moeda ISO 4217 e parser decimal manual estrito. | Evita ponto flutuante e arredondamento. Testar limites, overflow em parse/soma/subtração/negação, zero e moeda incompatível. |
| D02 | PostgreSQL com `pgx`, SQL explícito e transação coordenada pelo caso de uso. | Permite inspecionar locks, limites da transação e constraints. Repositórios recebem o mesmo `pgx.Tx`. |
| D03 | Lock pessimista `SELECT ... FOR UPDATE` por carteira, índices/constraints como segunda linha de defesa. | Serializa escritores da mesma carteira sem travar carteiras diferentes. Validar com três processos e `go test -race`. |
| D04 | `wager_transactions` separa origem `INTERNAL`/`EXTERNAL` por constraints e índice único de abertura por carteira. | OPENING não pode entrar por HTTP/SQS nem duplicar crédito inicial. Campos externos são obrigatórios apenas para origem externa. |
| D05 | Transações processadas persistem `result_balance_amount`, `result_balance_currency`, status e failure code; índice único por chave e ID externos dentro do provedor. | Replays reproduzem resultado original, detectam hash diferente e bloqueiam nova chave para a mesma operação. |
| D06 | Hash SHA-256 sobre JSON canônico com chaves ordenadas e apenas campos de negócio: providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money e referenceExternalTransactionId quando presente. Normalizar UUID e moeda; decimal aceito é serializado como duas casas. | Mesma operação via HTTP/SQS produz mesmo hash. Chave de idempotência, envelope e transporte ficam fora. Definir vetores de teste cruzados. |
| D07 | Uma reversão processada por referência, com exclusão mútua entre REFUND e ROLLBACK quando a referência é BET; ROLLBACK de WIN/REFUND ocorre no máximo uma vez. | Impede crédito duplo, inclusive em corrida. Um rollback de REFUND não reabre a possibilidade de refund da BET. |
| D08 | Referência ausente ou pendente usa `PENDING_REFERENCE` com `next_attempt_at`, contador, lease e TTL de 24 horas; backoff exponencial limitado a 5 minutos. | Retomada por outra instância. O TTL prevalece; exaustão gera `REFERENCE_NOT_FOUND` ou `REFERENCE_UNRESOLVED`, conforme o caso. Configuração e testes fixarão o limite de tentativas. |
| D09 | Falhas de negócio confirmadas têm códigos estáveis; `INSUFFICIENT_FUNDS_BET` difere de `INSUFFICIENT_FUNDS_REVERSAL`. Erros de infraestrutura transitórios não finalizam a operação. | Preserva semântica para cliente, auditoria e retry. Códigos adicionais incluem `REFERENCE_NOT_FOUND`, `REFERENCE_UNRESOLVED`, `REFERENCE_FAILED`, `REFERENCE_MISMATCH`, `ALREADY_REVERSED`, `WALLET_MISMATCH`, `CURRENCY_MISMATCH`. |
| D10 | Inbox SQS e efeito financeiro compartilham um commit; messageId do envelope identifica a mensagem e seu hash é comparado no replay. | Delete só após commit. Duplicata com corpo diferente é erro permanente auditável e enviada à DLQ. |
| D11 | Outbox usa snapshots de eventos tipados, `eventId` estável, claim SQL com `SKIP LOCKED` e lease; publicação ocorre após commit. | Crash após envio pode republicar o mesmo `eventId`. Consumidores externos devem deduplicar por `eventId`. |
| D12 | Fila de entrada SQS FIFO é acessível só ao produtor interno autenticado; chamadas HTTP recebem identidade de provedor pelo IdP, que a vincula ao pedido encaminhado. | Mensagens SQS não carregam uma identidade de remetente confiável por si. A política de broker impede provedores de publicar diretamente; testes e documentação verificam essa fronteira. |
| D13 | Endpoints de negócio validam JWT OIDC por JWKS e autorizam o `providerId` pelo claim; operações de carteira usam escopo interno. | Evita acesso cruzado em consultas/replays. Dados de outro provedor não devem ser expostos. |
| D14 | Saúde: liveness local; readiness consulta PostgreSQL e SQS com prazos curtos. Reconciliação lê carteira e ledger numa transação read-only de snapshot consistente. | Sinaliza dependências e evita falso desvio sob escrita concorrente. Divergência é registrada em resposta, log e métrica, sem corrigir saldo. |
| D15 | SQS FIFO usa `MessageGroupId=walletId` na entrada e `aggregateId` na saída, `MessageDeduplicationId=messageId/eventId`; nenhuma garantia depende disso. | Permite paralelismo entre carteiras e reentrega segura. HTTP e SQS ainda competem no lock da carteira. |
| D16 | Saldo inicial zero cria só carteira; saldo positivo cria OPENING, ledger e dois eventos no mesmo commit, mantendo versão 1. | Alinha versão e ledger desde a criação. Reabertura do mesmo `(playerId,currency)` retorna conflito. |

## Pontos a fechar durante revisão de contratos

- Forma exata dos claims OIDC, papéis de serviço e contas locais no realm provisionado.
- Códigos HTTP e envelopes de erro para rejeição, pendência, conflito e indisponibilidade.
- Esquema e política de versionamento dos eventos; nome/atributos da fila outbound.
- Número de tentativas de referência, consumer e publisher, visibility timeout e prazo de shutdown. Os limites devem ser configuráveis e testados.
- Versões exatas de Go, containers e dependências devem ser fixadas antes da implementação e repetidas em `go.mod`, Dockerfile e documentação.

## Execução autônoma e coordenação

| ID | Decisão | Motivo e verificação |
| --- | --- | --- |
| D17 | O módulo Go será `wagering`; pacotes internos importarão `wagering/internal/...`. | Identidade local independente, estável entre worktrees e sem acoplamento a hospedagem remota. |
| D18 | O Lead coordena uma Run Orca com tarefas DAG; workers usam worktrees isolados e commits locais por tarefa. | Evita edições concorrentes no mesmo checkout e torna revisão/integracão auditável. Não haverá push remoto. |
| D19 | Worker normal recebe seu item em `TASKS.yaml`, REQ IDs em `REQUIREMENTS.md`, decisões e handoffs pertinentes. Não recebe nem lê `SPEC.md`; revisor independente final recebe a cópia local imutável. | Reduz contexto e preserva uma fonte autoritativa para resolução de ambiguidade pelo Lead. |
| D20 | Modelos são selecionados por família e tier, sem IDs inventados. Codex `gpt-6.1-sol` aparece no cache local; Claude aceita o alias `sonnet`; Antigravity usa o default configurado quando lançado. A seleção efetiva será confirmada pelo recibo Orca. | Usa modelos econômicos onde adequados e verifica disponibilidade real. Astra fica reservado a falhas substantivas repetidas. |
| D21 | Cada handoff contém TASK, STATUS, COMMIT, IMPLEMENTED, FILES CHANGED, TESTS EXECUTED, DECISIONS / ASSUMPTIONS, KNOWN RISKS e NOTES FOR DEPENDENT TASKS. | O Lead só integra após ler o handoff, inspecionar o diff e verificar testes relevantes. |
| D22 | Não há gates humanos para decisões normais. Achados de revisão geram tarefas de reparo na mesma Run; Lead só para por bloqueio externo genuíno. | Executa a autorização explícita da fase de implementação. |
| D23 | Go 1.23.0 e Fx v1.24.0 foram fixados no bootstrap inicial. | É a versão Go instalada localmente; T12 repete a versão no Dockerfile. |
| D24 | O Lead assumiu T01 após três falhas de detecção de readiness do Codex TUI no Orca; T02 foi redistribuída para Claude Sonnet/high pelo mesmo problema. | Evita repetir tentativas sem sinal de progresso. Os commits continuam por tarefa, e os workers restantes permanecem supervisionados. |
| D25 | IDs internos de carteira, transação, ledger e evento são UUID; provider, player, round, game, externalTransactionId e chave são strings opacas não vazias de até 255 bytes. Round e game são obrigatórios para operações externas. | Esclarece contratos entre domínio, schema e transporte sem exigir UUIDs onde o contrato não os especifica. |
| D26 | `encoding/json` ordena chaves de mapas para o hash SHA-256. Formas decimais equivalentes normalizam para duas casas; moeda para maiúsculas; UUID para minúsculas; identificadores opacos preservam grafia. | Garante mesmo hash por HTTP/SQS e permite replays equivalentes, sem arredondar. |
| D27 | A integração de T12 encontrou Keycloak e MiniStack marcados unhealthy porque as imagens não contêm `curl`, embora os endpoints respondam 200; além disso faltam serviço da aplicação, identidades separadas e controle verificável de credenciais SQS. A tarefa R12 corrige esses pontos antes dos testes dependentes. | Evidência: `docker inspect` mostrou exec curl ausente; `curl` do host retornou HTTP 200 nos dois serviços. |

### D28 — Verify real IdP claims before HTTP integration
R12's first realm repair passed Compose health checks, but real access tokens still lacked the provider and internal scope claims required by T05, and the outbound queue policy targeted the inbound queue ARN. R13 will fix these using a fresh realm import and real token inspection. The HTTP app is not yet wired, so dependency health is the current Compose gate.

### D29 — Use wall-clock update timestamps in SQL triggers
PostgreSQL `now()` is fixed at transaction start. A domain transaction created after `BEGIN` can therefore have `created_at` later than the SQL trigger's `updated_at`; rehydration rejects it. Wallet and transaction update triggers use `GREATEST(clock_timestamp(), OLD.updated_at, NEW.created_at)` so timestamps stay monotonic. The live use-case suite exposed and verifies this failure mode.

### D30 — Release every receipt in a cancelled SQS batch
SQS ReceiveMessage can return up to ten messages already hidden by a visibility timeout. On shutdown, a cancelled consumer now releases all unstarted receipts in that batch, and never deletes them, so another instance can retry immediately.

### D31 — Gate outbox claims by earlier unpublished aggregate events
FIFO order across multiple publishers requires a database claim predicate: a wallet event is ineligible while any earlier event for that aggregate remains unpublished, even if another process has leased it. The claim query now enforces this; a real PostgreSQL test with two owners verifies no overtaking.

### D32 — Expose health and metrics on the application listener
Liveness is process-local; readiness performs bounded PostgreSQL ping and inbound SQS queue-attribute checks. `/metrics` exposes a dependency-free Prometheus text endpoint with bounded status labels and counters; HTTP request logs carry route, status, correlation and available provider/resource IDs; auth rejection logs include correlation ID; SQS logs carry message/correlation/transaction/wallet/provider IDs; outbox logs carry event/aggregate/correlation IDs. Reconciliation divergence is logged and metered. Logs exclude credentials and full financial payloads. Focused metrics tests pass; live readiness remains to be exercised in a network-enabled environment.

### D33 — Keep migration runner dependency-free
The embedded versioned SQL migration set is applied at startup and exposed through `cmd/migrate -direction up|down`, with a count for rollback. This avoids a second migration engine and keeps rollback behavior adjacent to the SQL assets. Compile and package tests pass; real up/down cycle needs the integration environment.

### D34 — Record bounded verification honestly under sandbox restrictions
Current execution cannot open sockets and the linked worktree Git common directory is read-only. Full and live integration verification and supervised dispatch are unavailable. The local recovery Git copy permits commits, but its refs are not yet reflected in the original shared repository. Keep T17 live execution and T19 open until runtime and filesystem capability return.

### D35 — Preserve original Orca gitdir while enabling local commits
The linked worktree's root `.git` file and shared common Git directory are read-only. Do not replace the pointer or mutate the shared repository. Create a no-hardlink local clone of Git metadata at `.agent/gitmeta`, select the original `carlosmeds/lead` commit, and operate against it through explicit `GIT_DIR` and `GIT_WORK_TREE`; save the original `.git` pointer in `.agent/ORIGINAL_GITDIR.txt`. This preserves every working file and original shared reference while allowing local commits and a future bundle/transfer. No push or main merge is permitted.

### D36 — Test crash windows through build-tag-only failpoints
`systemfault`-tagged builds pause only in two deterministic points: after a committed inbox operation but before SQS deletion, and after an outbox lease has committed but before send. Default builds use no-op hooks. This permits T17 to kill actual independent OS processes at the precise recovery boundaries without exposing environment-controlled pause behavior in production binaries. Both build variants must compile, and only a real infrastructure run can accept the scenarios.

### D37 — Count SQS financial replays separately from message deduplication
Inbox rows prevent duplicate transport work while the shared idempotency key prevents duplicate financial effects. A redelivered message that returns the persisted result increments the duplicate metric; a conflicting inbox/business identity increments the conflict metric. Logs include stable message/correlation and business identity IDs but never the operation amount or full body.

### D38 — Preserve and transfer recovery commits after Git access returns
The original linked worktree was read-only during the recovery run, so commits were created in a no-hardlink Git metadata clone. Once the environment restored Git writes, transfer those commits through a new local branch/worktree, preserving the original worktree's existing uncommitted files. Do not push or merge to main.

### D39 — Isolate multiprocess system tests with per-run queues
The Compose application also consumes the project's default inbound queue. Sharing that queue with T17 allows the normal Compose process, which uses a different database, to steal test messages. Each T17 run now creates unique real FIFO input and output queues and removes them at cleanup. Test subscenarios also ensure their required app-process count so a failed earlier scenario cannot cascade into a panic in later scenarios.
