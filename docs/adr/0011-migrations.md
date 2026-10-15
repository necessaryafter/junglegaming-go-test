# ADR 0011 — Migrations com golang-migrate

## Decisão

Usar `github.com/golang-migrate/migrate/v4`, com fonte `iofs` sobre os arquivos SQL
embarcados e driver PostgreSQL `pgx/v5`. A biblioteca descobre as versões pelos
nomes dos arquivos e gerencia ordenação, advisory lock e estado `dirty`.
O comando `cmd/migrate` continua separado da aplicação e aceita `up` e `down`.

Cada arquivo é enviado inteiro ao PostgreSQL, sem divisão por ponto e vírgula,
preservando funções PL/pgSQL e atomicidade por arquivo. As versões aplicadas
antes de uma falha permanecem aplicadas. A versão que falhou fica `dirty`;
novas execuções falham até a investigação e correção explícita do histórico.
Não expomos `force` como recuperação automática.
O readiness também recusa histórico `dirty` e schemas sem a constraint da
política de `FAILED` validada.

O contexto interrompe a sequência entre migrations. Timeouts de conexão,
statement e lock limitam operações em andamento, pois a biblioteca não recebe
o contexto do chamador em todas as chamadas. O runner fecha suas conexões e
preserva a conexão fornecida pelos fixtures e pelo comando.

## Compatibilidade

O runner anterior mantinha uma linha por versão em `schema_migrations`, sem
coluna `dirty`. Uma ponte de compatibilidade valida a sequência, adiciona a
coluna e mantém somente a versão mais recente, dentro de uma operação SQL
atômica protegida pelo lock usado pelo runner anterior. Bancos novos são
inicializados diretamente pela biblioteca. Históricos com `dirty` já existente
não são alterados pela ponte, mesmo quando marcados como incompletos.

A atualização pressupõe que executáveis antigos de migrations não continuem
rodando. Após converter a tabela, o runner antigo não deve ser reutilizado.
O histórico anterior só registra versões concluídas; sua conversão não tenta
adivinhar nem corrigir schemas inconsistentes.

## Motivo e verificação

Ordenação, locks e recuperação de falhas são responsabilidades de uma biblioteca
especializada. Mantemos no projeto apenas o SQL, a configuração da execução e a
ponte para o formato anterior.

Testes de integração cobrem up/down repetidos, atualização do formato anterior
sem perda de dados, preservação dos contadores da outbox e bloqueio de up/down
após uma migration falhar e deixar a versão `dirty`.
