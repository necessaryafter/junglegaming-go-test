# Otimização das validações financeiras

## Mudanças mantidas

- A migration reversível `000004_wallet_validation` adiciona índices em
  `wager_transactions(wallet_id)` e `wallet_ledger(transaction_id)`.
- As funções, consultas e disparos dos triggers financeiros permanecem iguais.
- Permanecem os locks por carteira, constraints, imutabilidade, idempotência,
  inbox/outbox atômicas e retomada de referências. Não há flag de sessão nem
  desativação de verificações.

Decisão e limites na [ADR 0003](../adr/0003-transacoes-e-concorrencia.md).

## Estado da comparação

A execução final foi interrompida a pedido do usuário antes de completar as
três repetições de cada versão. Os casos concluídos passaram sem erros HTTP
inesperados, com reconciliação correta e drenagem confirmada da outbox:

| Variante | Repetição | Req/s | p95 ms | p99 ms |
| --- | ---: | ---: | ---: | ---: |
| Anterior | 1 | 168,27 | 180,84 | 255,30 |
| Com índices | 1 | 182,09 | 163,03 | 481,40 |
| Com índices | 2 | 148,81 | 234,44 | 488,34 |

A amostra é parcial e **não comprova ganho sustentado de vazão ou latência**. A outbox
continuou exigindo espera significativa: 119,31 s na primeira execução anterior
e 89,23 s na primeira com índices, incluindo a espera pelo coletor.
Os relatórios e planos dos três casos estão na
[evidência JSON](otimizacao-resultados.json). Os arquivos brutos estão em
`.local/load/optimization-final-20261015/`; o manifesto registra
`completed=false` e `interrupted_by_user=true`.

Não houve nova rodada de testes após a interrupção. A migration final ainda não
foi aplicada ao banco principal do ambiente local.

## Comparação reproduzível

Com o Compose existente saudável e sua outbox vazia, compile as duas versões com
o mesmo toolchain e flags. A revisão anterior às otimizações é `52ae055`:

```sh
mkdir -p .local/baseline .local/bin
git archive 52ae055 | tar -x -C .local/baseline
CGO_ENABLED=0 go -C .local/baseline build -trimpath -ldflags='-s -w' -o ../bin/jungle-baseline ./cmd/jungle
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o .local/bin/jungle-optimized ./cmd/jungle
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o .local/bin/migrate-optimized ./cmd/migrate
python3 scripts/compare-load.py \
  --baseline-binary .local/bin/jungle-baseline \
  --optimized-binary .local/bin/jungle-optimized \
  --migrate-binary .local/bin/migrate-optimized \
  --wallet-counts 8 --repeats 3 --duration 15 \
  --output-dir .local/load/comparacao
```

O script usa Podman por padrão. Para Docker, acrescente `--engine docker` e
informe os nomes dos containers em `--postgres` e `--app`. O usuário do banco
local precisa permitir dump, restore e criação/exclusão de bancos de teste.
As portas `18081`–`18083` precisam estar livres; `--port` altera a primeira.

Cada caso restaura o mesmo snapshot inicial em um banco temporário, executa
`ANALYZE` e inicia três containers com os mesmos parâmetros e credenciais de
runtime do container de origem. A migration 4 é aplicada somente à variante
otimizada. A mudança final contém apenas índices; por isso, os dois binários
compilados têm o mesmo código Go de produção. Alternamos a ordem das variantes entre repetições. Não há execução
simultânea dos casos. Os containers e bancos temporários são removidos ao final
de cada caso; o banco de origem é apenas lido.

São 16 clientes em modelo fechado, três segundos de aquecimento e 15 segundos
de medição. O histórico cresce durante cada fase; versões mais rápidas acabam
validando mais lançamentos. O gerador mantém as mesmas verificações financeiras
e mede a drenagem da outbox separadamente. O broker de saída e o IdP são
compartilhados; não são restaurados entre casos. A máquina também é compartilhada.

O diretório de saída guarda snapshot, hashes dos binários, manifesto, relatórios
HTTP, logs e planos SQL coletados fora das fases medidas. Os detalhes do gerador
estão na [metodologia de carga](carga.md#metodologia). Não se trata de capacidade
máxima nem de um teste estatístico de significância.

## Experimentos descartados

Antes da versão final, foram avaliadas duas mudanças adicionais:

1. Acrescentar a carteira aos joins induziu planos com repetição das leituras do
   mesmo histórico. No primeiro caso, houve sete respostas `503`; a reconciliação
   permaneceu correta. O ajuste nos joins foi retirado.
2. Eliminar o INSERT de `PENDING` e persistir diretamente o resultado reduzia de
   quatro para três os disparos da validação. Com os índices finais, o diagnóstico
   de 30 segundos apresentou 15 respostas `503`, enquanto a versão anterior
   passou no mesmo tipo de diagnóstico. A escrita intermediária foi restaurada.

A amostragem de `pg_stat_activity` registrou espera pelo lock da carteira e
esperas `WalSync`/`WALWriteLock` em commits financeiros e da outbox. Essas
observações não isolam a causa de cada timeout nem demonstram que o comportamento
seria igual em outro host. Não houve divergência financeira nesses ensaios.

Os testes de domínio e recuperação haviam passado com a escrita única, mas o
comparativo HTTP não sustentou sua adoção. Consolidar validações continua sendo
uma proposta futura, exigindo prova das invariantes e comparação de carga.
