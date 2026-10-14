# ADR 0005 — Referências pendentes e reversões

## Decisão

Persistir `PENDING_REFERENCE` quando a referência não chegou ou ainda está pendente.
O worker retenta com backoff de 1 a 256 segundos e prazo de 24 horas. Ele bloqueia
carteira antes de transação, como o fluxo de entrada, e usa `SKIP LOCKED`.
Referência sem sucesso ou expirada resulta em rejeição definitiva.

Reversões são integrais e exigem o mesmo provedor, jogador, carteira, moeda e rodada.
`REFUND` e `ROLLBACK` disputam uma única reversão bem-sucedida da referência.
Desfazer um refund não libera a aposta original para nova devolução.
Estados terminais não mudam; reidratar não reaplica movimentos.

## Motivo

Persistir a espera permite receber mensagens fora de ordem e continuar após
um reinício. Limitar cada referência a uma reversão impede devoluções repetidas
por ciclos de refund e rollback. A rejeição persistida mantém o resultado de replays;
corrigir a operação exige outro ID externo e outra chave.

## Verificação e detalhes do prazo

A [matriz de reversões](../matriz-reversoes.md) cobre as regras no domínio e
seus efeitos duráveis no PostgreSQL, incluindo disputas entre pools independentes.
Uma reversão rejeitada não ocupa a vaga da referência; uma nova operação pode
ser processada depois de recompor o saldo. O replay da rejeição permanece rejeitado.

O prazo de 24 horas é contado desde a criação da operação que aguarda referência.
No retry, se a referência continuar ausente ou pendente após esse prazo, a operação
é rejeitada com `REFERENCE_NOT_FOUND`. Se já estiver processada quando o worker
retomar, a operação é avaliada normalmente, mesmo após 24 horas. Uma referência
rejeitada ou falha resulta em `REFERENCE_UNSUCCESSFUL`. Uma rejeição por expiração
não é reaberta quando a referência chega depois.
