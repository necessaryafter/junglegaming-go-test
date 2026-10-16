# Carga HTTP e custo das validações por carteira

Os números desta página registram a implementação anterior às otimizações.
A [comparação das validações](otimizacao-validacoes.md) documenta a mudança e
o procedimento para comparar as versões usando cópias do mesmo banco.

O gerador `scripts/load.py` usa apenas Python 3.10+ e sua biblioteca padrão.
Reutiliza conexões HTTP por thread, autentica no Keycloak e renova tokens antes
que expirem. Não recebe tokens ou segredos pela linha de comando; secrets locais
podem ser substituídos pelas variáveis `PROVIDER_A_SECRET` e `JUNGLE_INTERNAL_SECRET`.

## Preparação e execução

Inicie o ambiente conforme o README e habilite três aplicações:

```sh
docker compose --profile replicas up --build -d
python3 scripts/load-matrix.py
```

Para Podman, depois do [provisionamento sequencial](../guias/operacao.md#inicialização):

```sh
podman-compose --profile replicas build app-2 app-3
podman-compose --profile replicas up --no-deps -d app-2 app-3
python3 scripts/load-matrix.py
```

A matriz padrão executa oito casos sequenciais: escrita com 1 e 8 carteiras para
históricos iniciais de 0, 100 e 1.000 entradas por carteira, mais replay e conflito
de idempotência. Cada caso mede 15 segundos com 16 clientes concorrentes, após um
segundo de aquecimento, com limite de 600 segundos para confirmar drenagem.
Preparo, aquecimento, reconciliação e espera pela outbox
ficam fora do throughput e dos percentis. O preparo de históricos maiores pode
levar vários minutos. Para mudar os parâmetros:

```sh
python3 scripts/load-matrix.py \
  --apis http://localhost:8081,http://localhost:8082,http://localhost:8083 \
  --histories 0,100,1000 --wallet-counts 1,8 \
  --concurrency 16 --duration 30 --warmup 3 \
  --output-dir .local/load/minha-matriz
```

Um caso isolado:

```sh
python3 scripts/load.py --history 1000 --wallets 1 --concurrency 16 \
  --duration 30 --scenario write --output .local/load/carteira-disputada.json
python3 -m unittest discover -s scripts -p test_load.py -v
```

Use volumes novos e um projeto Compose dedicado para comparar execuções com
bases equivalentes. O teste cria carteiras e transações sintéticas duráveis;
não apaga nem altera registros financeiros existentes. `.local/` contém os
relatórios brutos e fica fora do Git e do contexto de build.

### Retomar um preparo caro

O gerador salva `ARQUIVO.fixture.json` antes de esperar a outbox. Se o preparo
atingir o limite de drenagem, o JSON principal registra a etapa e o caminho da
fixture. Depois de drenar, retome sem criar mais carteiras ou eventos de preparo:

```sh
python3 scripts/load.py --wallets 8 --history 1000 --scenario write \
  --apis http://localhost:8081,http://localhost:8082,http://localhost:8083 \
  --concurrency 16 --duration 15 --warmup 0 --drain-timeout 600 \
  --fixture .local/load/minha-matriz/write-w8-h1000.fixture.json \
  --output .local/load/minha-matriz/write-w8-h1000-retomado.json
```

Use somente fixtures sintéticas criadas pelo gerador. A retomada exige mesmos
cenário, histórico e quantidade de carteiras, não repete aquecimento e compara
identidade, saldo e versão de cada carteira ao estado salvo. Se alguma operação
já tiver alterado uma carteira, o gerador recusa a fixture. Uma fixture usada
em escrita medida não pode ser reutilizada novamente com o estado anterior.

## Metodologia

- Modelo fechado: cada cliente aguarda a resposta antes de enviar outra operação.
  Não há taxa fixa de chegada; os resultados não medem uma capacidade máxima nem
  corrigem omissão coordenada. O gerador e os serviços compartilham a máquina.
- Abertura em `1000000.00 BRL`; histórico semeado com WIN de `0.01`; escritas
  medidas com BET de `0.01`. Dinheiro usa strings e inteiros, inclusive nas
  verificações do gerador. Floats são usados somente para tempo e telemetria.
- Escritas têm IDs e chaves próprios. Replay repete uma operação confirmada;
  conflito altera seu valor mantendo a identidade. `409 CONFLICT` é resultado
  esperado apenas no cenário de conflito, separado de erros inesperados.
- O histórico cresce durante aquecimento e medição. `baseline` registra o tamanho
  real no início; `verification` registra o tamanho ao final. Os tiers não são
  históricos estáticos nem um benchmark isolado de SQL.
- Tráfego de entrada HTTP, publicação pela outbox no SQS e coleta de observabilidade
  reais. Esta matriz não mede throughput de entrada do consumidor SQS.
- Latência é a duração observada pelo cliente por chamada, incluindo falhas.
  p50/p95/p99 usam nearest rank sobre todas as amostras; não são médias dos
  percentis de instâncias. O tempo total inclui a conclusão de chamadas iniciadas
  antes do limite. Até 100.000 chamadas por fase limitam o uso de memória.
- Não há retry automático de operações financeiras. Falhas de transporte/5xx são
  reportadas; identidades com resultado incerto são consultadas após a fase.
  Erros inesperados fazem o caso terminar com código diferente de zero.
- Toda carteira é reconciliada e comparada ao saldo, versão e quantidade exata de
  lançamentos esperados a partir das operações confirmadas. Replay e conflitos
  devem manter o estado financeiro. Inconsistências fazem o caso falhar.

## Telemetria e evidências

Cada JSON registra parâmetros, instantes UTC, preparo, throughput, p50/p95/p99,
status HTTP, resultados, erros, taxa de erro, conflitos da aplicação e banco,
amostras da outbox e reconciliações. O manifesto da matriz acrescenta revisão Git,
indicação de alterações locais, CPU, memória, sistema e versão Python.

As métricas são amostradas diretamente de cada `/metrics`, a cada segundo, sem
credenciais de banco. A coleta da aplicação atualiza o backlog a cada 15
segundos: os máximos são observados, não máximos exatos, e a idade tem resolução
em segundos. O backlog é global e usa o maior valor observado, sem somar a mesma
outbox entre instâncias. Contadores usam deltas por instância e registram resets.
Falhas de coleta permanecem explícitas. Antes de medir e ao terminar, o gerador
aguarda uma coleta nova que confirme outbox vazia, com limite configurável (`--drain-timeout`: 300 segundos no caso isolado,
600 na matriz).
O tempo até confirmar uma coleta nova com outbox vazia inclui a espera pelo
coletor; não é o tempo exato de publicação de cada evento. As amostras da
drenagem são reportadas separadamente; `drain_telemetry`
mostra o atraso observado após a fase HTTP.

Para examinar o custo SQL, `scripts/load-validations.sql` reproduz as três consultas
de validação de `check_wallet_ledger()`, em transação somente leitura:

```sh
psql "$DATABASE_URL" -v wallet_id=UUID_DA_CARTEIRA_DO_RELATORIO \
  -f scripts/load-validations.sql > .local/load/validacoes.txt
```

Use uma credencial com leitura das tabelas. `EXPLAIN (ANALYZE, BUFFERS)` mostra
linhas, buffers e tempos reais. Execute fora da fase medida e repita para reduzir
a influência de cache. Esses tempos não incluem locks, escrita, autenticação,
rede ou o custo completo de COMMIT. A função é chamada por triggers diferidos;
uma transação pode executá-la várias vezes. Não se pode subtrair seus tempos
isolados da latência HTTP para obter uma decomposição exata.

O script acompanha as consultas da migration atual. Os planos históricos abaixo
foram coletados antes da migration 4; o comparador executa as consultas
correspondentes a cada versão e salva os planos separadamente.

## Grafana e logs

Acompanhe [a dashboard local](http://localhost:3000/d/jungle-overview) durante a
matriz. Para logs JSON no Grafana, habilite o perfil `logs` conforme o
[guia de observabilidade](../guias/observabilidade.md#logs-no-grafana).

Os resultados medidos desta implementação são registrados abaixo; dependem do
hardware e do estado da base, sem meta mínima de RPS no desafio.

## Medição local de 15/10/2026

Evidência consolidada: [carga-resultados.json](carga-resultados.json). Relatórios
brutos, logs e fixtures desta execução estão em `.local/load/matrix-20261015/`.

Ambiente: Ryzen 5 5500, 6 núcleos/12 CPUs lógicas, aproximadamente 15,5 GiB de RAM,
Linux 7.2.4, Python 3.14.7 e Podman 6.1.1. Três containers da aplicação com a mesma
imagem, PostgreSQL 17.2, MiniStack 1.5.27, Keycloak 26.8.0, Prometheus 3.8.1,
Grafana 12.3.1, Loki 3.7.0 e Alloy 1.14.0. Sem limites de CPU/memória nos containers.
Base da aplicação: revisão `52ae055`; ferramentas de carga e logs eram alterações
locais durante a medição. A base já continha testes anteriores: não é uma medição
em banco vazio nem em hardware dedicado.

Todos os casos abaixo tiveram zero erros HTTP inesperados e reconciliação correta.
Os `409` do último caso são conflitos esperados, com zero novas movimentações.
O histórico indicado é o preparo nominal; os tamanhos reais estão no JSON.

| Caso | Carteiras | Histórico/carteira | Req/s | p50 ms | p95 ms | p99 ms | Maior idade observada da outbox (s) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Escrita | 1 | 0 | 31.34 | 360.84 | 1422.97 | 2021.66 | 3 |
| Escrita | 8 | 0 | 182.24 | 63.28 | 205.77 | 513.34 | 57 |
| Escrita | 1 | 100 | 28.37 | 634.08 | 1323.39 | 1696.79 | 0 |
| Escrita | 8 | 100 | 126.88 | 100.04 | 297.26 | 456.70 | 70 |
| Escrita | 1 | 1000 | 17.65 | 1158.25 | 1597.67 | 1711.91 | 0 |
| Escrita | 8 | 1000 | 93.39 | 152.34 | 317.56 | 518.84 | 30 |
| Replay | 1 | 0 | 3889.20 | 3.67 | 7.69 | 10.40 | 0 |
| Conflito | 1 | 0 | 3716.14 | 3.96 | 7.67 | 10.10 | 0 |

A idade usa o maior valor observado diretamente ou no Prometheus entre a fase e
sua espera final. Amostras históricas do Prometheus só entram se o timestamp da
coleta do banco for posterior ao início da fase; isso evita atribuir backlog
antigo ao caso seguinte. Zero observado não comprova publicação instantânea.
As séries têm resolução limitada e podem perder picos entre coletas/scrapes.

### Falhas de drenagem preservadas

A execução inicial da matriz **não passou integralmente**:

- Escrita em oito carteiras sem histórico: as 2.786 chamadas retornaram `200` e
  os oito ledgers reconciliaram, mas a outbox não foi confirmada vazia em 60
  segundos. O resultado permanece com `passed=false`, sem apagar essa falha.
- O preparo de oito carteiras com 1.000 entradas gerou 16.000 eventos apenas
  para o histórico e atingiu o limite de 300 segundos antes da fase medida.
  As carteiras foram preservadas, conferidas e retomadas com limite de 600
  segundos, sem duplicar o histórico. A retomada processou 1.423 chamadas sem
  erros, reconciliou as oito carteiras e confirmou outbox vazia após a fase.

O caso com oito carteiras e histórico de 100 confirmou drenagem em 84,29 segundos;
a retomada com histórico de 1.000, em 45,16 segundos. Essas durações incluem a
espera pela próxima coleta de backlog. Ao terminar a carga, o banco não tinha
eventos pendentes. O limite final de 600 segundos da matriz permite separar os
casos neste emulador, sem corrigir a capacidade de publicação observada.

### Probes SQL

Cinco execuções retidas por carteira, após descartar uma primeira execução:

| Entradas no ledger | Saldo/versão: mediana ms | Compatibilidade do lançamento: mediana ms | Operação processada com ledger: mediana ms |
| ---: | ---: | ---: | ---: |
| 2 | 0.213 | 0.108 | 57.176 |
| 536 | 63.139 | 5.997 | 59.934 |
| 575 | 64.374 | 6.243 | 65.324 |
| 1306 | 69.893 | 13.253 | 68.519 |

Os planos contêm varreduras globais, incluindo `Seq Scan` de aproximadamente
16,7 mil entradas do ledger na terceira validação, mesmo para a carteira com
apenas dois lançamentos. Portanto, o custo também depende do tamanho total das
tabelas e do plano escolhido, além do histórico da carteira. Os planos e buffers
estão no JSON; os tempos não representam a execução completa dos triggers.

Esta é uma caracterização inicial, com uma amostra por cenário. Não demonstra
capacidade máxima nem significância estatística da diferença entre cenários.
Próximas investigações: índices/consultas das validações, custo de COMMIT e vazão
de publicação com SQS real, preservando as mesmas invariantes e testes.
