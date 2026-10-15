# ADR 0010 — Falhas permanentes e estado FAILED

## Decisão

`FAILED` é terminal e representa exclusivamente uma falha de infraestrutura
comprovadamente irrecuperável. O único código admitido é
`INFRASTRUCTURE_DATA_LOSS`: perda definitiva de dados necessários para executar
a operação com segurança, após investigação e confirmação de que backups,
reentrega e reconstrução auditável não podem recuperá-los.

Um exemplo é uma operação pendente cuja referência foi perdida por um incidente
de armazenamento, com perda comprovada também nas fontes de recuperação. Uma
referência simplesmente ausente ainda segue espera e expiração normais.

O registro exige identidade e payload válidos, ausência de movimento financeiro
da operação e certeza sobre o resultado do commit anterior. Um timeout de commit
não prova rollback: primeiro consulte a identidade persistida. Se o estado
financeiro não puder ser determinado, mantenha a investigação; registrar FAILED
não resolve uma divergência financeira.

## Classificação

| Situação | Tratamento |
| --- | --- |
| Saldo insuficiente, limite monetário, referência incompatível ou já revertida | `REJECTED`, com o código de negócio correspondente |
| Referência ainda ausente | `PENDING_REFERENCE`; após 24 horas, `REJECTED` / `REFERENCE_NOT_FOUND` |
| Timeout, cancelamento, deadlock, desconexão, banco ou broker indisponível | Rollback quando possível; HTTP 503 ou retry SQS |
| Erro de constraint, trigger, schema, configuração ou permissão | Rollback e investigação/correção; SQS mantém retry/redrive |
| Entrada inválida, token expirado, acesso proibido ou conflito de identidade | Erro de entrada/autorização/conflito; SQS mantém retry/redrive, sem criar FAILED |
| Tentativas esgotadas ou mensagem na DLQ | Investigação e possível reprocessamento; não alteram o estado financeiro |
| Falha de publicação da outbox | Retry da publicação; preserva a transação financeira já confirmada |
| Perda irrecuperável de dados confirmada, com resultado financeiro conhecido | `FAILED` / `INFRASTRUCTURE_DATA_LOSS`, mediante decisão operacional auditada |

## Garantias e limites

O domínio valida o código tanto na transição de `PENDING` ou
`PENDING_REFERENCE` quanto na restauração. A constraint do banco aplica a mesma
regra e impede usar esse código em `REJECTED`. Estados terminais permanecem
imutáveis. Replay retorna o resultado persistido sem reaplicar a operação; uma
referência FAILED provoca `REFERENCE_UNSUCCESSFUL` na operação dependente.

Nenhuma falha observável automaticamente no fluxo atual comprova perda
irrecuperável. Por isso, o serviço não converte erros PostgreSQL, SQS ou OIDC em
FAILED. Não há endpoint, comando administrativo ou worker de encerramento manual
nessa entrega. Uma futura ferramenta deve exigir evidência auditável do incidente,
autorização interna, lock da carteira, verificação da ausência de ledger da
operação e persistência atômica da decisão. O teste SQL simula uma decisão já
investigada; não é um procedimento operacional de encerramento.

## Verificação

Testes de domínio cobrem causas permitidas e recusadas em transições e
restauração, além da imutabilidade terminal. O teste de integração
`TestFailedPolicyAndReplay` cobre a constraint, replay entre pools com e sem
metadados de inbox e reconciliação sem movimento financeiro adicional.
Os testes existentes de recuperação após DLQ continuam comprovando que uma
falha de escrita corrigível permite reprocessamento posterior.
